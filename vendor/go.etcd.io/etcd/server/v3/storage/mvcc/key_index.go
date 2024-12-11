// Copyright 2015 The etcd Authors
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

package mvcc

import (
	"bytes"
	"errors"
	"fmt"

	"go.uber.org/zap"
)

var (
	ErrRevisionNotFound = errors.New("mvcc: revision not found")
)

// keyIndex stores the revisions of a key in the backend.
// Each keyIndex has at least one key generation.
// Each generation might have several key versions.
// Tombstone on a key appends a tombstone version at the end
// of the current generation and creates a new empty generation.
// Each version of a key has an index pointing to the backend.
//
// For example: put(1.0);put(2.0);tombstone(3.0);put(4.0);tombstone(5.0) on key "foo"
// generate a keyIndex:
// key:     "foo"
// modified: 5
// generations:
//
//	{empty}
//	{4.0, 5.0(t)}
//	{1.0, 2.0, 3.0(t)}
//
// Compact a keyIndex removes the versions with smaller or equal to
// rev except the largest one. If the generation becomes empty
// during compaction, it will be removed. if all the generations get
// removed, the keyIndex should be removed.
//
// For example:
// compact(2) on the previous example
// generations:
//
//	{empty}
//	{4.0, 5.0(t)}
//	{2.0, 3.0(t)}
//
// compact(4)
// generations:
//
//	{empty}
//	{4.0, 5.0(t)}
//
// compact(5):
// generations:
//
//	{empty}
//	{5.0(t)}
//
// compact(6):
// generations:
//
//	{empty} -> key SHOULD be removed.
type keyIndex struct {
	key         []byte
	modified    Revision // the main rev of the last modification
	generations []generation
}

// put puts a revision to the keyIndex.
func (ki *keyIndex) put(lg *zap.Logger, main int64, sub int64) {
	rev := Revision{Main: main, Sub: sub}

	if !rev.GreaterThan(ki.modified) {
		lg.Panic(
			"'put' with an unexpected smaller revision",
			zap.Int64("given-revision-main", rev.Main),
			zap.Int64("given-revision-sub", rev.Sub),
			zap.Int64("modified-revision-main", ki.modified.Main),
			zap.Int64("modified-revision-sub", ki.modified.Sub),
		)
	}
	if len(ki.generations) == 0 {
		ki.generations = append(ki.generations, generation{})
	}
	g := &ki.generations[len(ki.generations)-1]
	if len(g.revs) == 0 { // create a new key
		keysGauge.Inc()
		g.created = rev
	}
	g.revs = append(g.revs, rev)
	g.ver++
	ki.modified = rev
}

func (ki *keyIndex) restore(lg *zap.Logger, created, modified Revision, ver int64) {
	if len(ki.generations) != 0 {
		lg.Panic(
			"'restore' got an unexpected non-empty generations",
			zap.Int("generations-size", len(ki.generations)),
		)
	}

	ki.modified = modified
	g := generation{created: created, ver: ver, revs: []Revision{modified}}
	ki.generations = append(ki.generations, g)
	keysGauge.Inc()
}

// tombstone puts a revision, pointing to a tombstone, to the keyIndex.
// It also creates a new empty generation in the keyIndex.
// It returns ErrRevisionNotFound when tombstone on an empty generation.
func (ki *keyIndex) tombstone(lg *zap.Logger, main int64, sub int64) error {
	if ki.isEmpty() {
		lg.Panic(
			"'tombstone' got an unexpected empty keyIndex",
			zap.String("key", string(ki.key)),
		)
	}
	if ki.generations[len(ki.generations)-1].isEmpty() {
		return ErrRevisionNotFound
	}
	ki.put(lg, main, sub)
	ki.generations = append(ki.generations, generation{})
	keysGauge.Dec()
	return nil
}

// get gets the modified, created revision and version of the key that satisfies the given atRev.
// Rev must be smaller than or equal to the given atRev.
func (ki *keyIndex) get(lg *zap.Logger, atRev int64) (modified, created Revision, ver int64, err error) {
	if ki.isEmpty() {
		lg.Panic(
			"'get' got an unexpected empty keyIndex",
			zap.String("key", string(ki.key)),
		)
	}
	g := ki.findGeneration(atRev)
	if g.isEmpty() {
		return Revision{}, Revision{}, 0, ErrRevisionNotFound
	}

	n := g.walk(func(rev Revision) bool { return rev.Main > atRev })
	if n != -1 {
		return g.revs[n], g.created, g.ver - int64(len(g.revs)-n-1), nil
	}

	return Revision{}, Revision{}, 0, ErrRevisionNotFound
}

// since returns revisions since the given rev. Only the revision with the
// largest sub revision will be returned if multiple revisions have the same
// main revision.
func (ki *keyIndex) since(lg *zap.Logger, rev int64) []Revision {
	if ki.isEmpty() {
		lg.Panic(
			"'since' got an unexpected empty keyIndex",
			zap.String("key", string(ki.key)),
		)
	}
	since := Revision{Main: rev}
	var gi int
	// find the generations to start checking
	for gi = len(ki.generations) - 1; gi > 0; gi-- {
		g := ki.generations[gi]
		if g.isEmpty() {
			continue
		}
		if since.GreaterThan(g.created) {
			break
		}
	}

	var revs []Revision
	var last int64
	for ; gi < len(ki.generations); gi++ {
		for _, r := range ki.generations[gi].revs {
			if since.GreaterThan(r) {
				continue
			}
			if r.Main == last {
				// replace the revision with a new one that has higher sub value,
				// because the original one should not be seen by external
				revs[len(revs)-1] = r
				continue
			}
			revs = append(revs, r)
			last = r.Main
		}
	}
	return revs
}

// compact compacts a keyIndex by removing the versions with smaller or equal
// revision than the given atRev except the largest one.
// If a generation becomes empty during compaction, it will be removed.
func (ki *keyIndex) compact(lg *zap.Logger, atRev int64, available map[Revision]struct{}) {
	if ki.isEmpty() {
		lg.Panic(
			"'compact' got an unexpected empty keyIndex",
			zap.String("key", string(ki.key)),
		)
	}

	genIdx, revIndex := ki.doCompact(atRev, available)

	g := &ki.generations[genIdx]
	if !g.isEmpty() {
		// remove the previous contents.
		if revIndex != -1 {
			g.revs = g.revs[revIndex:]
		}
	}

	// remove the previous generations.
	ki.generations = ki.generations[genIdx:]
}

// keep finds the revision to be kept if compact is called at given atRev.
func (ki *keyIndex) keep(atRev int64, available map[Revision]struct{}) {
	if ki.isEmpty() {
		return
	}

	genIdx, revIndex := ki.doCompact(atRev, available)
	g := &ki.generations[genIdx]
	if !g.isEmpty() {
		// If the given `atRev` is a tombstone, we need to skip it.
		//
		// Note that this s different from the `compact` function which
		// keeps tombstone in such case. We need to stay consistent with
		// existing versions, ensuring they always generate the same hash
		// values.
		if revIndex == len(g.revs)-1 && genIdx != len(ki.generations)-1 {
			delete(available, g.revs[revIndex])
		}
	}
}

func (ki *keyIndex) doCompact(atRev int64, available map[Revision]struct{}) (genIdx int, revIndex int) {
	// walk until reaching the first revision smaller or equal to "atRev",
	// and add the revision to the available map
	f := func(rev Revision) bool {
		if rev.Main <= atRev {
			available[rev] = struct{}{}
			return false
		}
		return true
	}

	genIdx, g := 0, &ki.generations[0]
	// find first generation includes atRev or created after atRev
	for genIdx < len(ki.generations)-1 {
		if tomb := g.revs[len(g.revs)-1].Main; tomb >= atRev {
			break
		}
		genIdx++
		g = &ki.generations[genIdx]
	}

	revIndex = g.walk(f)

	return genIdx, revIndex
}

func (ki *keyIndex) isEmpty() bool {
	return len(ki.generations) == 1 && ki.generations[0].isEmpty()
}

// findGeneration finds out the generation of the keyIndex that the
// given rev belongs to. If the given rev is at the gap of two generations,
// which means that the key does not exist at the given rev, it returns nil.
func (ki *keyIndex) findGeneration(rev int64) *generation {
	lastg := len(ki.generations) - 1
	cg := lastg

	for cg >= 0 {
		if len(ki.generations[cg].revs) == 0 {
			cg--
			continue
		}
		g := ki.generations[cg]
		if cg != lastg {
			if tomb := g.revs[len(g.revs)-1].Main; tomb <= rev {
				return nil
			}
		}
		if g.revs[0].Main <= rev {
			return &ki.generations[cg]
		}
		cg--
	}
	return nil
}

func (ki *keyIndex) Less(bki *keyIndex) bool {
	return bytes.Compare(ki.key, bki.key) == -1
}

func (ki *keyIndex) equal(b *keyIndex) bool {
	if !bytes.Equal(ki.key, b.key) {
		return false
	}
	if ki.modified != b.modified {
		return false
	}
	if len(ki.generations) != len(b.generations) {
		return false
	}
	for i := range ki.generations {
		ag, bg := ki.generations[i], b.generations[i]
		if !ag.equal(bg) {
			return false
		}
	}
	return true
}

func (ki *keyIndex) String() string {
	var s string
	for _, g := range ki.generations {
		s += g.String()
	}
	return s
}

// generation contains multiple revisions of a key.
type generation struct {
	ver     int64
	created Revision // when the generation is created (put in first revision).
	revs    []Revision
}

func (g *generation) isEmpty() bool { return g == nil || len(g.revs) == 0 }

// walk walks through the revisions in the generation in descending order.
// It passes the revision to the given function.
// walk returns until: 1. it finishes walking all pairs 2. the function returns false.
// walk returns the position at where it stopped. If it stopped after
// finishing walking, -1 will be returned.
func (g *generation) walk(f func(rev Revision) bool) int {
	l := len(g.revs)
	for i := range g.revs {
		ok := f(g.revs[l-i-1])
		if !ok {
			return l - i - 1
		}
	}
	return -1
}

func (g *generation) String() string {
	return fmt.Sprintf("g: created[%d] ver[%d], revs %#v\n", g.created, g.ver, g.revs)
}

func (g generation) equal(b generation) bool {
	if g.ver != b.ver {
		return false
	}
	if len(g.revs) != len(b.revs) {
		return false
	}

	for i := range g.revs {
		ar, br := g.revs[i], b.revs[i]
		if ar != br {
			return false
		}
	}
	return true
}
