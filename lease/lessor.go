// Copyright 2015 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package lease

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/coreos/etcd/lease/leasepb"
	"github.com/coreos/etcd/pkg/idutil"
	"github.com/coreos/etcd/storage/backend"
)

const (
	// NoLease is a special LeaseID representing the absence of a lease.
	NoLease = LeaseID(0)
)

var (
	minLeaseTerm = 5 * time.Second

	leaseBucketName = []byte("lease")
	forever         = time.Unix(math.MaxInt64, 0)

	ErrNotPrimary = errors.New("not a primary lessor")
)

type LeaseID int64

// DeleteableRange defines an interface with DeleteRange method.
// We define this interface only for lessor to limit the number
// of methods of storage.KV to what lessor actually needs.
//
// Having a minimum interface makes testing easy.
type DeleteableRange interface {
	DeleteRange(key, end []byte) (int64, int64)
}

// A Lessor is the owner of leases. It can grant, revoke, renew and modify leases for lessee.
type Lessor interface {
	// Grant grants a lease that expires at least after TTL seconds.
	Grant(ttl int64) *Lease
	// Revoke revokes a lease with given ID. The item attached to the
	// given lease will be removed. If the ID does not exist, an error
	// will be returned.
	Revoke(id LeaseID) error

	// Promote promotes the lessor to be the primary lessor. Primary lessor manages
	// the expiration and renew of leases.
	Promote()

	// Demote demotes the lessor from being the primary lessor.
	Demote()

	// Renew renews a lease with given ID.  If the ID does not exist, an error
	// will be returned.
	Renew(id LeaseID) error

	// ExpiredLeasesC returens a chan that is used to receive expired leases.
	ExpiredLeasesC() <-chan []*Lease
}

// lessor implements Lessor interface.
// TODO: use clockwork for testability.
type lessor struct {
	mu sync.Mutex

	// primary indicates if this lessor is the primary lessor. The primary
	// lessor manages lease expiration and renew.
	//
	// in etcd, raft leader is the primary. Thus there might be two primary
	// leaders at the same time (raft allows concurrent leader but with different term)
	// for at most a leader election timeout.
	// The old primary leader cannot affect the correctness since its proposal has a
	// smaller term and will not be committed.
	//
	// TODO: raft follower do not forward lease management proposals. There might be a
	// very small window (within second normally which depends on go scheduling) that
	// a raft follow is the primary between the raft leader demotion and lessor demotion.
	// Usually this should not be a problem. Lease should not be that sensitive to timing.
	primary bool

	// TODO: probably this should be a heap with a secondary
	// id index.
	// Now it is O(N) to loop over the leases to find expired ones.
	// We want to make Grant, Revoke, and FindExpired all O(logN) and
	// Renew O(1).
	// FindExpired and Renew should be the most frequent operations.
	leaseMap map[LeaseID]*Lease

	// A DeleteableRange the lessor operates on.
	// When a lease expires, the lessor will delete the
	// leased range (or key) from the DeleteableRange.
	dr DeleteableRange

	// backend to persist leases. We only persist lease ID and expiry for now.
	// The leased items can be recovered by iterating all the keys in kv.
	b backend.Backend

	expiredC chan []*Lease

	idgen *idutil.Generator
}

func NewLessor(lessorID uint8, b backend.Backend, dr DeleteableRange) Lessor {
	return newLessor(lessorID, b, dr)
}

func newLessor(lessorID uint8, b backend.Backend, dr DeleteableRange) *lessor {
	// ensure the most significant bit of lessorID is 0.
	// so all the IDs generated by id generator will be greater than 0.
	if int8(lessorID) < 0 {
		lessorID = uint8(-int8(lessorID))
	}

	l := &lessor{
		leaseMap: make(map[LeaseID]*Lease),
		b:        b,
		dr:       dr,
		// expiredC is a small buffered chan to avoid unncessary blocking.
		expiredC: make(chan []*Lease, 16),
		idgen:    idutil.NewGenerator(lessorID, time.Now()),
	}
	l.initAndRecover()

	go l.runLoop()

	return l
}

// TODO: when lessor is under high load, it should give out lease
// with longer TTL to reduce renew load.
func (le *lessor) Grant(ttl int64) *Lease {
	// TODO: define max TTL
	expiry := time.Now().Add(time.Duration(ttl) * time.Second)
	expiry = minExpiry(time.Now(), expiry)

	id := LeaseID(le.idgen.Next())

	le.mu.Lock()
	defer le.mu.Unlock()

	l := &Lease{ID: id, TTL: ttl, expiry: expiry, itemSet: make(map[leaseItem]struct{})}
	if _, ok := le.leaseMap[id]; ok {
		panic("lease: unexpected duplicate ID!")
	}

	le.leaseMap[id] = l
	l.persistTo(le.b)

	return l
}

func (le *lessor) Revoke(id LeaseID) error {
	le.mu.Lock()
	defer le.mu.Unlock()

	l := le.leaseMap[id]
	if l == nil {
		return fmt.Errorf("lease: cannot find lease %x", id)
	}

	for item := range l.itemSet {
		le.dr.DeleteRange([]byte(item.key), nil)
	}

	delete(le.leaseMap, l.ID)
	l.removeFrom(le.b)

	return nil
}

// Renew renews an existing lease. If the given lease does not exist or
// has expired, an error will be returned.
// TODO: return new TTL?
func (le *lessor) Renew(id LeaseID) error {
	le.mu.Lock()
	defer le.mu.Unlock()

	if !le.primary {
		return ErrNotPrimary
	}

	l := le.leaseMap[id]
	if l == nil {
		return fmt.Errorf("lease: cannot find lease %x", id)
	}

	expiry := time.Now().Add(time.Duration(l.TTL) * time.Second)
	l.expiry = minExpiry(time.Now(), expiry)
	return nil
}

func (le *lessor) Promote() {
	le.mu.Lock()
	defer le.mu.Unlock()

	le.primary = true

	// refresh the expiries of all leases.
	for _, l := range le.leaseMap {
		l.expiry = minExpiry(time.Now(), time.Now().Add(time.Duration(l.TTL)*time.Second))
	}
}

func (le *lessor) Demote() {
	le.mu.Lock()
	defer le.mu.Unlock()

	// set the expiries of all leases to forever
	for _, l := range le.leaseMap {
		l.expiry = forever
	}

	le.primary = false
}

// Attach attaches items to the lease with given ID. When the lease
// expires, the attached items will be automatically removed.
// If the given lease does not exist, an error will be returned.
func (le *lessor) Attach(id LeaseID, items []leaseItem) error {
	le.mu.Lock()
	defer le.mu.Unlock()

	l := le.leaseMap[id]
	if l == nil {
		return fmt.Errorf("lease: cannot find lease %x", id)
	}

	for _, it := range items {
		l.itemSet[it] = struct{}{}
	}
	return nil
}

func (le *lessor) Recover(b backend.Backend, dr DeleteableRange) {
	le.mu.Lock()
	defer le.mu.Unlock()

	le.b = b
	le.dr = dr
	le.leaseMap = make(map[LeaseID]*Lease)

	le.initAndRecover()
}

func (le *lessor) ExpiredLeasesC() <-chan []*Lease {
	return le.expiredC
}

func (le *lessor) runLoop() {
	// TODO: stop runLoop
	for {
		var ls []*Lease

		le.mu.Lock()
		if le.primary {
			ls = le.findExpiredLeases()
		}
		le.mu.Unlock()

		if len(ls) != 0 {
			select {
			case le.expiredC <- ls:
			default:
				// the receiver of expiredC is probably busy handling
				// other stuff
				// let's try this next time after 500ms
			}
		}

		time.Sleep(500 * time.Millisecond)
	}
}

// findExpiredLeases loops all the leases in the leaseMap and returns the expired
// leases that needed to be revoked.
func (le *lessor) findExpiredLeases() []*Lease {
	leases := make([]*Lease, 0, 16)
	now := time.Now()

	for _, l := range le.leaseMap {
		// TODO: probably should change to <= 100-500 millisecond to
		// make up committing latency.
		if l.expiry.Sub(now) <= 0 {
			leases = append(leases, l)
		}
	}

	return leases
}

// get gets the lease with given id.
// get is a helper fucntion for testing, at least for now.
func (le *lessor) get(id LeaseID) *Lease {
	le.mu.Lock()
	defer le.mu.Unlock()

	return le.leaseMap[id]
}

func (le *lessor) initAndRecover() {
	tx := le.b.BatchTx()
	tx.Lock()

	tx.UnsafeCreateBucket(leaseBucketName)
	_, vs := tx.UnsafeRange(leaseBucketName, int64ToBytes(0), int64ToBytes(math.MaxInt64), 0)
	// TODO: copy vs and do decoding outside tx lock if lock contention becomes an issue.
	for i := range vs {
		var lpb leasepb.Lease
		err := lpb.Unmarshal(vs[i])
		if err != nil {
			tx.Unlock()
			panic("failed to unmarshal lease proto item")
		}
		ID := LeaseID(lpb.ID)
		le.leaseMap[ID] = &Lease{
			ID:  ID,
			TTL: lpb.TTL,

			// itemSet will be filled in when recover key-value pairs
			// set expiry to forever, refresh when promoted
			expiry: forever,
		}
	}
	tx.Unlock()

	le.b.ForceCommit()
}

type Lease struct {
	ID  LeaseID
	TTL int64 // time to live in seconds

	itemSet map[leaseItem]struct{}
	// expiry time in unixnano
	expiry time.Time
}

func (l Lease) persistTo(b backend.Backend) {
	key := int64ToBytes(int64(l.ID))

	lpb := leasepb.Lease{ID: int64(l.ID), TTL: int64(l.TTL)}
	val, err := lpb.Marshal()
	if err != nil {
		panic("failed to marshal lease proto item")
	}

	b.BatchTx().Lock()
	b.BatchTx().UnsafePut(leaseBucketName, key, val)
	b.BatchTx().Unlock()
}

func (l Lease) removeFrom(b backend.Backend) {
	key := int64ToBytes(int64(l.ID))

	b.BatchTx().Lock()
	b.BatchTx().UnsafeDelete(leaseBucketName, key)
	b.BatchTx().Unlock()
}

type leaseItem struct {
	key string
}

// minExpiry returns a minimal expiry. A minimal expiry is the larger on
// between now + minLeaseTerm and the given expectedExpiry.
func minExpiry(now time.Time, expectedExpiry time.Time) time.Time {
	minExpiry := time.Now().Add(minLeaseTerm)
	if expectedExpiry.Sub(minExpiry) < 0 {
		expectedExpiry = minExpiry
	}
	return expectedExpiry
}

func int64ToBytes(n int64) []byte {
	bytes := make([]byte, 8)
	binary.BigEndian.PutUint64(bytes, uint64(n))
	return bytes
}
