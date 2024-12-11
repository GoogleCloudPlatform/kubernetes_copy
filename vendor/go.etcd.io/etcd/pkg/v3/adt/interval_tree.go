// Copyright 2016 The etcd Authors
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

package adt

import (
	"bytes"
	"fmt"
	"math"
	"strings"
)

// Comparable is an interface for trichotomic comparisons.
type Comparable interface {
	// Compare gives the result of a 3-way comparison
	// a.Compare(b) = 1 => a > b
	// a.Compare(b) = 0 => a == b
	// a.Compare(b) = -1 => a < b
	Compare(c Comparable) int
}

type rbcolor int

const (
	black rbcolor = iota
	red
)

func (c rbcolor) String() string {
	switch c {
	case black:
		return "black"
	case red:
		return "red"
	default:
		panic(fmt.Errorf("unknown color %d", c))
	}
}

// Interval implements a Comparable interval [begin, end)
// TODO: support different sorts of intervals: (a,b), [a,b], (a, b]
type Interval struct {
	Begin Comparable
	End   Comparable
}

// Compare on an interval gives == if the interval overlaps.
func (ivl *Interval) Compare(c Comparable) int {
	ivl2 := c.(*Interval)
	ivbCmpBegin := ivl.Begin.Compare(ivl2.Begin)
	ivbCmpEnd := ivl.Begin.Compare(ivl2.End)
	iveCmpBegin := ivl.End.Compare(ivl2.Begin)

	// ivl is left of ivl2
	if ivbCmpBegin < 0 && iveCmpBegin <= 0 {
		return -1
	}

	// iv is right of iv2
	if ivbCmpEnd >= 0 {
		return 1
	}

	return 0
}

type intervalNode struct {
	// iv is the interval-value pair entry.
	iv IntervalValue
	// max endpoint of all descendent nodes.
	max Comparable
	// left and right are sorted by low endpoint of key interval
	left, right *intervalNode
	// parent is the direct ancestor of the node
	parent *intervalNode
	c      rbcolor
}

func (x *intervalNode) color(sentinel *intervalNode) rbcolor {
	if x == sentinel {
		return black
	}
	return x.c
}

func (x *intervalNode) height(sentinel *intervalNode) int {
	if x == sentinel {
		return 0
	}
	ld := x.left.height(sentinel)
	rd := x.right.height(sentinel)
	if ld < rd {
		return rd + 1
	}
	return ld + 1
}

func (x *intervalNode) min(sentinel *intervalNode) *intervalNode {
	for x.left != sentinel {
		x = x.left
	}
	return x
}

// successor is the next in-order node in the tree
func (x *intervalNode) successor(sentinel *intervalNode) *intervalNode {
	if x.right != sentinel {
		return x.right.min(sentinel)
	}
	y := x.parent
	for y != sentinel && x == y.right {
		x = y
		y = y.parent
	}
	return y
}

// updateMax updates the maximum values for a node and its ancestors
func (x *intervalNode) updateMax(sentinel *intervalNode) {
	for x != sentinel {
		oldmax := x.max
		max := x.iv.Ivl.End
		if x.left != sentinel && x.left.max.Compare(max) > 0 {
			max = x.left.max
		}
		if x.right != sentinel && x.right.max.Compare(max) > 0 {
			max = x.right.max
		}
		if oldmax.Compare(max) == 0 {
			break
		}
		x.max = max
		x = x.parent
	}
}

type nodeVisitor func(n *intervalNode) bool

// visit will call a node visitor on each node that overlaps the given interval
func (x *intervalNode) visit(iv *Interval, sentinel *intervalNode, nv nodeVisitor) bool {
	if x == sentinel {
		return true
	}
	v := iv.Compare(&x.iv.Ivl)
	switch {
	case v < 0:
		if !x.left.visit(iv, sentinel, nv) {
			return false
		}
	case v > 0:
		maxiv := Interval{x.iv.Ivl.Begin, x.max}
		if maxiv.Compare(iv) == 0 {
			if !x.left.visit(iv, sentinel, nv) || !x.right.visit(iv, sentinel, nv) {
				return false
			}
		}
	default:
		if !x.left.visit(iv, sentinel, nv) || !nv(x) || !x.right.visit(iv, sentinel, nv) {
			return false
		}
	}
	return true
}

// IntervalValue represents a range tree node that contains a range and a value.
type IntervalValue struct {
	Ivl Interval
	Val any
}

// IntervalTree represents a (mostly) textbook implementation of the
// "Introduction to Algorithms" (Cormen et al, 3rd ed.) chapter 13 red-black tree
// and chapter 14.3 interval tree with search supporting "stabbing queries".
type IntervalTree interface {
	// Insert adds a node with the given interval into the tree.
	Insert(ivl Interval, val any)
	// Delete removes the node with the given interval from the tree, returning
	// true if a node is in fact removed.
	Delete(ivl Interval) bool
	// Len gives the number of elements in the tree.
	Len() int
	// Height is the number of levels in the tree; one node has height 1.
	Height() int
	// MaxHeight is the expected maximum tree height given the number of nodes.
	MaxHeight() int
	// Visit calls a visitor function on every tree node intersecting the given interval.
	// It will visit each interval [x, y) in ascending order sorted on x.
	Visit(ivl Interval, ivv IntervalVisitor)
	// Find gets the IntervalValue for the node matching the given interval
	Find(ivl Interval) *IntervalValue
	// Intersects returns true if there is some tree node intersecting the given interval.
	Intersects(iv Interval) bool
	// Contains returns true if the interval tree's keys cover the entire given interval.
	Contains(ivl Interval) bool
	// Stab returns a slice with all elements in the tree intersecting the interval.
	Stab(iv Interval) []*IntervalValue
	// Union merges a given interval tree into the receiver.
	Union(inIvt IntervalTree, ivl Interval)
}

// NewIntervalTree returns a new interval tree.
func NewIntervalTree() IntervalTree {
	sentinel := &intervalNode{
		iv:     IntervalValue{},
		max:    nil,
		left:   nil,
		right:  nil,
		parent: nil,
		c:      black,
	}
	return &intervalTree{
		root:     sentinel,
		count:    0,
		sentinel: sentinel,
	}
}

type intervalTree struct {
	root  *intervalNode
	count int

	// red-black NIL node
	// use 'sentinel' as a dummy object to simplify boundary conditions
	// use the sentinel to treat a nil child of a node x as an ordinary node whose parent is x
	// use one shared sentinel to represent all nil leaves and the root's parent
	sentinel *intervalNode
}

// TODO: make this consistent with textbook implementation
//
// "Introduction to Algorithms" (Cormen et al, 3rd ed.), chapter 13.4, p324
//
//	  RB-DELETE(T, z)
//
//	  y = z
//	  y-original-color = y.color
//
//	  if z.left == T.nil
//	  	x = z.right
//	  	RB-TRANSPLANT(T, z, z.right)
//	  else if z.right == T.nil
//	  	x = z.left
//	 	RB-TRANSPLANT(T, z, z.left)
//	  else
//	 	y = TREE-MINIMUM(z.right)
//	 	y-original-color = y.color
//	 	x = y.right
//	 	if y.p == z
//	 		x.p = y
//	 	else
//	 		RB-TRANSPLANT(T, y, y.right)
//	 		y.right = z.right
//	 		y.right.p = y
//	 	RB-TRANSPLANT(T, z, y)
//	 	y.left = z.left
//	 	y.left.p = y
//	 	y.color = z.color
//
//	  if y-original-color == BLACK
//	  	RB-DELETE-FIXUP(T, x)

// Delete removes the node with the given interval from the tree, returning
// true if a node is in fact removed.
func (ivt *intervalTree) Delete(ivl Interval) bool {
	z := ivt.find(ivl)
	if z == ivt.sentinel {
		return false
	}

	y := z
	if z.left != ivt.sentinel && z.right != ivt.sentinel {
		y = z.successor(ivt.sentinel)
	}

	x := ivt.sentinel
	if y.left != ivt.sentinel {
		x = y.left
	} else if y.right != ivt.sentinel {
		x = y.right
	}

	x.parent = y.parent

	if y.parent == ivt.sentinel {
		ivt.root = x
	} else {
		if y == y.parent.left {
			y.parent.left = x
		} else {
			y.parent.right = x
		}
		y.parent.updateMax(ivt.sentinel)
	}
	if y != z {
		z.iv = y.iv
		z.updateMax(ivt.sentinel)
	}

	if y.color(ivt.sentinel) == black {
		ivt.deleteFixup(x)
	}

	ivt.count--
	return true
}

// "Introduction to Algorithms" (Cormen et al, 3rd ed.), chapter 13.4, p326
//
//	RB-DELETE-FIXUP(T, z)
//
//	while x ≠ T.root and x.color == BLACK
//		if x == x.p.left
//			w = x.p.right
//			if w.color == RED
//				w.color = BLACK
//				x.p.color = RED
//				LEFT-ROTATE(T, x, p)
//			if w.left.color == BLACK and w.right.color == BLACK
//				w.color = RED
//				x = x.p
//			else if w.right.color == BLACK
//					w.left.color = BLACK
//					w.color = RED
//					RIGHT-ROTATE(T, w)
//					w = w.p.right
//				w.color = x.p.color
//				x.p.color = BLACK
//				LEFT-ROTATE(T, w.p)
//				x = T.root
//		else
//			w = x.p.left
//			if w.color == RED
//				w.color = BLACK
//				x.p.color = RED
//				RIGHT-ROTATE(T, x, p)
//			if w.right.color == BLACK and w.left.color == BLACK
//				w.color = RED
//				x = x.p
//			else if w.left.color == BLACK
//					w.right.color = BLACK
//					w.color = RED
//					LEFT-ROTATE(T, w)
//					w = w.p.left
//				w.color = x.p.color
//				x.p.color = BLACK
//				RIGHT-ROTATE(T, w.p)
//				x = T.root
//
//	x.color = BLACK
func (ivt *intervalTree) deleteFixup(x *intervalNode) {
	for x != ivt.root && x.color(ivt.sentinel) == black {
		if x == x.parent.left { // line 3-20
			w := x.parent.right
			if w.color(ivt.sentinel) == red {
				w.c = black
				x.parent.c = red
				ivt.rotateLeft(x.parent)
				w = x.parent.right
			}
			if w == nil {
				break
			}
			if w.left.color(ivt.sentinel) == black && w.right.color(ivt.sentinel) == black {
				w.c = red
				x = x.parent
			} else {
				if w.right.color(ivt.sentinel) == black {
					w.left.c = black
					w.c = red
					ivt.rotateRight(w)
					w = x.parent.right
				}
				w.c = x.parent.color(ivt.sentinel)
				x.parent.c = black
				w.right.c = black
				ivt.rotateLeft(x.parent)
				x = ivt.root
			}
		} else { // line 22-38
			// same as above but with left and right exchanged
			w := x.parent.left
			if w.color(ivt.sentinel) == red {
				w.c = black
				x.parent.c = red
				ivt.rotateRight(x.parent)
				w = x.parent.left
			}
			if w == nil {
				break
			}
			if w.left.color(ivt.sentinel) == black && w.right.color(ivt.sentinel) == black {
				w.c = red
				x = x.parent
			} else {
				if w.left.color(ivt.sentinel) == black {
					w.right.c = black
					w.c = red
					ivt.rotateLeft(w)
					w = x.parent.left
				}
				w.c = x.parent.color(ivt.sentinel)
				x.parent.c = black
				w.left.c = black
				ivt.rotateRight(x.parent)
				x = ivt.root
			}
		}
	}

	if x != nil {
		x.c = black
	}
}

func (ivt *intervalTree) createIntervalNode(ivl Interval, val any) *intervalNode {
	return &intervalNode{
		iv:     IntervalValue{ivl, val},
		max:    ivl.End,
		c:      red,
		left:   ivt.sentinel,
		right:  ivt.sentinel,
		parent: ivt.sentinel,
	}
}

// TODO: make this consistent with textbook implementation
//
// "Introduction to Algorithms" (Cormen et al, 3rd ed.), chapter 13.3, p315
//
//	 RB-INSERT(T, z)
//
//	 y = T.nil
//	 x = T.root
//
//	 while x ≠ T.nil
//	 	y = x
//	 	if z.key < x.key
//	 		x = x.left
//	 	else
//	 		x = x.right
//
//	 z.p = y
//
//	 if y == T.nil
//	 	T.root = z
//	 else if z.key < y.key
//	 	y.left = z
//	 else
//	 	y.right = z
//
//	 z.left = T.nil
//	 z.right = T.nil
//	 z.color = RED
//
//	 RB-INSERT-FIXUP(T, z)

// Insert adds a node with the given interval into the tree.
func (ivt *intervalTree) Insert(ivl Interval, val any) {
	y := ivt.sentinel
	z := ivt.createIntervalNode(ivl, val)
	x := ivt.root
	for x != ivt.sentinel {
		y = x
		if z.iv.Ivl.Begin.Compare(x.iv.Ivl.Begin) < 0 {
			x = x.left
		} else {
			x = x.right
		}
	}

	z.parent = y
	if y == ivt.sentinel {
		ivt.root = z
	} else {
		if z.iv.Ivl.Begin.Compare(y.iv.Ivl.Begin) < 0 {
			y.left = z
		} else {
			y.right = z
		}
		y.updateMax(ivt.sentinel)
	}
	z.c = red

	ivt.insertFixup(z)
	ivt.count++
}

// "Introduction to Algorithms" (Cormen et al, 3rd ed.), chapter 13.3, p316
//
//	RB-INSERT-FIXUP(T, z)
//
//	while z.p.color == RED
//		if z.p == z.p.p.left
//			y = z.p.p.right
//			if y.color == RED
//				z.p.color = BLACK
//				y.color = BLACK
//				z.p.p.color = RED
//				z = z.p.p
//			else if z == z.p.right
//					z = z.p
//					LEFT-ROTATE(T, z)
//				z.p.color = BLACK
//				z.p.p.color = RED
//				RIGHT-ROTATE(T, z.p.p)
//		else
//			y = z.p.p.left
//			if y.color == RED
//				z.p.color = BLACK
//				y.color = BLACK
//				z.p.p.color = RED
//				z = z.p.p
//			else if z == z.p.right
//					z = z.p
//					RIGHT-ROTATE(T, z)
//				z.p.color = BLACK
//				z.p.p.color = RED
//				LEFT-ROTATE(T, z.p.p)
//
//	T.root.color = BLACK
func (ivt *intervalTree) insertFixup(z *intervalNode) {
	for z.parent.color(ivt.sentinel) == red {
		if z.parent == z.parent.parent.left { // line 3-15
			y := z.parent.parent.right
			if y.color(ivt.sentinel) == red {
				y.c = black
				z.parent.c = black
				z.parent.parent.c = red
				z = z.parent.parent
			} else {
				if z == z.parent.right {
					z = z.parent
					ivt.rotateLeft(z)
				}
				z.parent.c = black
				z.parent.parent.c = red
				ivt.rotateRight(z.parent.parent)
			}
		} else { // line 16-28
			// same as then with left/right exchanged
			y := z.parent.parent.left
			if y.color(ivt.sentinel) == red {
				y.c = black
				z.parent.c = black
				z.parent.parent.c = red
				z = z.parent.parent
			} else {
				if z == z.parent.left {
					z = z.parent
					ivt.rotateRight(z)
				}
				z.parent.c = black
				z.parent.parent.c = red
				ivt.rotateLeft(z.parent.parent)
			}
		}
	}

	// line 30
	ivt.root.c = black
}

// rotateLeft moves x so it is left of its right child
//
// "Introduction to Algorithms" (Cormen et al, 3rd ed.), chapter 13.2, p313
//
//	LEFT-ROTATE(T, x)
//
//	y = x.right
//	x.right = y.left
//
//	if y.left ≠ T.nil
//		y.left.p = x
//
//	y.p = x.p
//
//	if x.p == T.nil
//		T.root = y
//	else if x == x.p.left
//		x.p.left = y
//	else
//		x.p.right = y
//
//	y.left = x
//	x.p = y
func (ivt *intervalTree) rotateLeft(x *intervalNode) {
	// rotateLeft x must have right child
	if x.right == ivt.sentinel {
		return
	}

	// line 2-3
	y := x.right
	x.right = y.left

	// line 5-6
	if y.left != ivt.sentinel {
		y.left.parent = x
	}
	x.updateMax(ivt.sentinel)

	// line 10-15, 18
	ivt.replaceParent(x, y)

	// line 17
	y.left = x
	y.updateMax(ivt.sentinel)
}

// rotateRight moves x so it is right of its left child
//
//	RIGHT-ROTATE(T, x)
//
//	y = x.left
//	x.left = y.right
//
//	if y.right ≠ T.nil
//		y.right.p = x
//
//	y.p = x.p
//
//	if x.p == T.nil
//		T.root = y
//	else if x == x.p.right
//		x.p.right = y
//	else
//		x.p.left = y
//
//	y.right = x
//	x.p = y
func (ivt *intervalTree) rotateRight(x *intervalNode) {
	// rotateRight x must have left child
	if x.left == ivt.sentinel {
		return
	}

	// line 2-3
	y := x.left
	x.left = y.right

	// line 5-6
	if y.right != ivt.sentinel {
		y.right.parent = x
	}
	x.updateMax(ivt.sentinel)

	// line 10-15, 18
	ivt.replaceParent(x, y)

	// line 17
	y.right = x
	y.updateMax(ivt.sentinel)
}

// replaceParent replaces x's parent with y
func (ivt *intervalTree) replaceParent(x *intervalNode, y *intervalNode) {
	y.parent = x.parent
	if x.parent == ivt.sentinel {
		ivt.root = y
	} else {
		if x == x.parent.left {
			x.parent.left = y
		} else {
			x.parent.right = y
		}
		x.parent.updateMax(ivt.sentinel)
	}
	x.parent = y
}

// Len gives the number of elements in the tree
func (ivt *intervalTree) Len() int { return ivt.count }

// Height is the number of levels in the tree; one node has height 1.
func (ivt *intervalTree) Height() int { return ivt.root.height(ivt.sentinel) }

// MaxHeight is the expected maximum tree height given the number of nodes
func (ivt *intervalTree) MaxHeight() int {
	return int((2 * math.Log2(float64(ivt.Len()+1))) + 0.5)
}

// IntervalVisitor is used on tree searches; return false to stop searching.
type IntervalVisitor func(n *IntervalValue) bool

// Visit calls a visitor function on every tree node intersecting the given interval.
// It will visit each interval [x, y) in ascending order sorted on x.
func (ivt *intervalTree) Visit(ivl Interval, ivv IntervalVisitor) {
	ivt.root.visit(&ivl, ivt.sentinel, func(n *intervalNode) bool { return ivv(&n.iv) })
}

// find the exact node for a given interval
func (ivt *intervalTree) find(ivl Interval) *intervalNode {
	ret := ivt.sentinel
	f := func(n *intervalNode) bool {
		if n.iv.Ivl != ivl {
			return true
		}
		ret = n
		return false
	}
	ivt.root.visit(&ivl, ivt.sentinel, f)
	return ret
}

// Find gets the IntervalValue for the node matching the given interval
func (ivt *intervalTree) Find(ivl Interval) (ret *IntervalValue) {
	n := ivt.find(ivl)
	if n == ivt.sentinel {
		return nil
	}
	return &n.iv
}

// Intersects returns true if there is some tree node intersecting the given interval.
func (ivt *intervalTree) Intersects(iv Interval) bool {
	x := ivt.root
	for x != ivt.sentinel && iv.Compare(&x.iv.Ivl) != 0 {
		if x.left != ivt.sentinel && x.left.max.Compare(iv.Begin) > 0 {
			x = x.left
		} else {
			x = x.right
		}
	}
	return x != ivt.sentinel
}

// Contains returns true if the interval tree's keys cover the entire given interval.
func (ivt *intervalTree) Contains(ivl Interval) bool {
	var maxEnd, minBegin Comparable

	isContiguous := true
	ivt.Visit(ivl, func(n *IntervalValue) bool {
		if minBegin == nil {
			minBegin = n.Ivl.Begin
			maxEnd = n.Ivl.End
			return true
		}
		if maxEnd.Compare(n.Ivl.Begin) < 0 {
			isContiguous = false
			return false
		}
		if n.Ivl.End.Compare(maxEnd) > 0 {
			maxEnd = n.Ivl.End
		}
		return true
	})

	return isContiguous && minBegin != nil && maxEnd.Compare(ivl.End) >= 0 && minBegin.Compare(ivl.Begin) <= 0
}

// Stab returns a slice with all elements in the tree intersecting the interval.
func (ivt *intervalTree) Stab(iv Interval) (ivs []*IntervalValue) {
	if ivt.count == 0 {
		return nil
	}
	f := func(n *IntervalValue) bool { ivs = append(ivs, n); return true }
	ivt.Visit(iv, f)
	return ivs
}

// Union merges a given interval tree into the receiver.
func (ivt *intervalTree) Union(inIvt IntervalTree, ivl Interval) {
	f := func(n *IntervalValue) bool {
		ivt.Insert(n.Ivl, n.Val)
		return true
	}
	inIvt.Visit(ivl, f)
}

type visitedInterval struct {
	root  Interval
	left  Interval
	right Interval
	color rbcolor
	depth int
}

func (vi visitedInterval) String() string {
	bd := new(strings.Builder)
	bd.WriteString(fmt.Sprintf("root [%v,%v,%v], left [%v,%v], right [%v,%v], depth %d",
		vi.root.Begin, vi.root.End, vi.color,
		vi.left.Begin, vi.left.End,
		vi.right.Begin, vi.right.End,
		vi.depth,
	))
	return bd.String()
}

// visitLevel traverses tree in level order.
// used for testing
func (ivt *intervalTree) visitLevel() []visitedInterval {
	if ivt.root == ivt.sentinel {
		return nil
	}

	rs := make([]visitedInterval, 0, ivt.Len())

	type pair struct {
		node  *intervalNode
		depth int
	}
	queue := []pair{{ivt.root, 0}}
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]

		vi := visitedInterval{
			root:  f.node.iv.Ivl,
			color: f.node.color(ivt.sentinel),
			depth: f.depth,
		}
		if f.node.left != ivt.sentinel {
			vi.left = f.node.left.iv.Ivl
			queue = append(queue, pair{f.node.left, f.depth + 1})
		}
		if f.node.right != ivt.sentinel {
			vi.right = f.node.right.iv.Ivl
			queue = append(queue, pair{f.node.right, f.depth + 1})
		}

		rs = append(rs, vi)
	}

	return rs
}

type StringComparable string

func (s StringComparable) Compare(c Comparable) int {
	sc := c.(StringComparable)
	if s < sc {
		return -1
	}
	if s > sc {
		return 1
	}
	return 0
}

func NewStringInterval(begin, end string) Interval {
	return Interval{StringComparable(begin), StringComparable(end)}
}

func NewStringPoint(s string) Interval {
	return Interval{StringComparable(s), StringComparable(s + "\x00")}
}

// StringAffineComparable treats "" as > all other strings
type StringAffineComparable string

func (s StringAffineComparable) Compare(c Comparable) int {
	sc := c.(StringAffineComparable)

	if len(s) == 0 {
		if len(sc) == 0 {
			return 0
		}
		return 1
	}
	if len(sc) == 0 {
		return -1
	}

	if s < sc {
		return -1
	}
	if s > sc {
		return 1
	}
	return 0
}

func NewStringAffineInterval(begin, end string) Interval {
	return Interval{StringAffineComparable(begin), StringAffineComparable(end)}
}

func NewStringAffinePoint(s string) Interval {
	return NewStringAffineInterval(s, s+"\x00")
}

func NewInt64Interval(a int64, b int64) Interval {
	return Interval{Int64Comparable(a), Int64Comparable(b)}
}

func newInt64EmptyInterval() Interval {
	return Interval{Begin: nil, End: nil}
}

func NewInt64Point(a int64) Interval {
	return Interval{Int64Comparable(a), Int64Comparable(a + 1)}
}

type Int64Comparable int64

func (v Int64Comparable) Compare(c Comparable) int {
	vc := c.(Int64Comparable)
	cmp := v - vc
	if cmp < 0 {
		return -1
	}
	if cmp > 0 {
		return 1
	}
	return 0
}

// BytesAffineComparable treats empty byte arrays as > all other byte arrays
type BytesAffineComparable []byte

func (b BytesAffineComparable) Compare(c Comparable) int {
	bc := c.(BytesAffineComparable)

	if len(b) == 0 {
		if len(bc) == 0 {
			return 0
		}
		return 1
	}
	if len(bc) == 0 {
		return -1
	}

	return bytes.Compare(b, bc)
}

func NewBytesAffineInterval(begin, end []byte) Interval {
	return Interval{BytesAffineComparable(begin), BytesAffineComparable(end)}
}

func NewBytesAffinePoint(b []byte) Interval {
	be := make([]byte, len(b)+1)
	copy(be, b)
	be[len(b)] = 0
	return NewBytesAffineInterval(b, be)
}
