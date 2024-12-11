package bbolt

import (
	"bytes"
	"fmt"
	"sort"

	"go.etcd.io/bbolt/errors"
	"go.etcd.io/bbolt/internal/common"
)

// Cursor represents an iterator that can traverse over all key/value pairs in a bucket
// in lexicographical order.
// Cursors see nested buckets with value == nil.
// Cursors can be obtained from a transaction and are valid as long as the transaction is open.
//
// Keys and values returned from the cursor are only valid for the life of the transaction.
//
// Changing data while traversing with a cursor may cause it to be invalidated
// and return unexpected keys and/or values. You must reposition your cursor
// after mutating data.
type Cursor struct {
	bucket *Bucket
	stack  []elemRef
}

// Bucket returns the bucket that this cursor was created from.
func (c *Cursor) Bucket() *Bucket {
	return c.bucket
}

// First moves the cursor to the first item in the bucket and returns its key and value.
// If the bucket is empty then a nil key and value are returned.
// The returned key and value are only valid for the life of the transaction.
func (c *Cursor) First() (key []byte, value []byte) {
	common.Assert(c.bucket.tx.db != nil, "tx closed")
	k, v, flags := c.first()
	if (flags & uint32(common.BucketLeafFlag)) != 0 {
		return k, nil
	}
	return k, v
}

func (c *Cursor) first() (key []byte, value []byte, flags uint32) {
	c.stack = c.stack[:0]
	p, n := c.bucket.pageNode(c.bucket.RootPage())
	c.stack = append(c.stack, elemRef{page: p, node: n, index: 0})
	c.goToFirstElementOnTheStack()

	// If we land on an empty page then move to the next value.
	// https://github.com/boltdb/bolt/issues/450
	if c.stack[len(c.stack)-1].count() == 0 {
		c.next()
	}

	k, v, flags := c.keyValue()
	if (flags & uint32(common.BucketLeafFlag)) != 0 {
		return k, nil, flags
	}
	return k, v, flags
}

// Last moves the cursor to the last item in the bucket and returns its key and value.
// If the bucket is empty then a nil key and value are returned.
// The returned key and value are only valid for the life of the transaction.
func (c *Cursor) Last() (key []byte, value []byte) {
	common.Assert(c.bucket.tx.db != nil, "tx closed")
	c.stack = c.stack[:0]
	p, n := c.bucket.pageNode(c.bucket.RootPage())
	ref := elemRef{page: p, node: n}
	ref.index = ref.count() - 1
	c.stack = append(c.stack, ref)
	c.last()

	// If this is an empty page (calling Delete may result in empty pages)
	// we call prev to find the last page that is not empty
	for len(c.stack) > 1 && c.stack[len(c.stack)-1].count() == 0 {
		c.prev()
	}

	if len(c.stack) == 0 {
		return nil, nil
	}

	k, v, flags := c.keyValue()
	if (flags & uint32(common.BucketLeafFlag)) != 0 {
		return k, nil
	}
	return k, v
}

// Next moves the cursor to the next item in the bucket and returns its key and value.
// If the cursor is at the end of the bucket then a nil key and value are returned.
// The returned key and value are only valid for the life of the transaction.
func (c *Cursor) Next() (key []byte, value []byte) {
	common.Assert(c.bucket.tx.db != nil, "tx closed")
	k, v, flags := c.next()
	if (flags & uint32(common.BucketLeafFlag)) != 0 {
		return k, nil
	}
	return k, v
}

// Prev moves the cursor to the previous item in the bucket and returns its key and value.
// If the cursor is at the beginning of the bucket then a nil key and value are returned.
// The returned key and value are only valid for the life of the transaction.
func (c *Cursor) Prev() (key []byte, value []byte) {
	common.Assert(c.bucket.tx.db != nil, "tx closed")
	k, v, flags := c.prev()
	if (flags & uint32(common.BucketLeafFlag)) != 0 {
		return k, nil
	}
	return k, v
}

// Seek moves the cursor to a given key using a b-tree search and returns it.
// If the key does not exist then the next key is used. If no keys
// follow, a nil key is returned.
// The returned key and value are only valid for the life of the transaction.
func (c *Cursor) Seek(seek []byte) (key []byte, value []byte) {
	common.Assert(c.bucket.tx.db != nil, "tx closed")

	k, v, flags := c.seek(seek)

	// If we ended up after the last element of a page then move to the next one.
	if ref := &c.stack[len(c.stack)-1]; ref.index >= ref.count() {
		k, v, flags = c.next()
	}

	if k == nil {
		return nil, nil
	} else if (flags & uint32(common.BucketLeafFlag)) != 0 {
		return k, nil
	}
	return k, v
}

// Delete removes the current key/value under the cursor from the bucket.
// Delete fails if current key/value is a bucket or if the transaction is not writable.
func (c *Cursor) Delete() error {
	if c.bucket.tx.db == nil {
		return errors.ErrTxClosed
	} else if !c.bucket.Writable() {
		return errors.ErrTxNotWritable
	}

	key, _, flags := c.keyValue()
	// Return an error if current value is a bucket.
	if (flags & common.BucketLeafFlag) != 0 {
		return errors.ErrIncompatibleValue
	}
	c.node().del(key)

	return nil
}

// seek moves the cursor to a given key and returns it.
// If the key does not exist then the next key is used.
func (c *Cursor) seek(seek []byte) (key []byte, value []byte, flags uint32) {
	// Start from root page/node and traverse to correct page.
	c.stack = c.stack[:0]
	c.search(seek, c.bucket.RootPage())

	// If this is a bucket then return a nil value.
	return c.keyValue()
}

// first moves the cursor to the first leaf element under the last page in the stack.
func (c *Cursor) goToFirstElementOnTheStack() {
	for {
		// Exit when we hit a leaf page.
		var ref = &c.stack[len(c.stack)-1]
		if ref.isLeaf() {
			break
		}

		// Keep adding pages pointing to the first element to the stack.
		var pgId common.Pgid
		if ref.node != nil {
			pgId = ref.node.inodes[ref.index].Pgid()
		} else {
			pgId = ref.page.BranchPageElement(uint16(ref.index)).Pgid()
		}
		p, n := c.bucket.pageNode(pgId)
		c.stack = append(c.stack, elemRef{page: p, node: n, index: 0})
	}
}

// last moves the cursor to the last leaf element under the last page in the stack.
func (c *Cursor) last() {
	for {
		// Exit when we hit a leaf page.
		ref := &c.stack[len(c.stack)-1]
		if ref.isLeaf() {
			break
		}

		// Keep adding pages pointing to the last element in the stack.
		var pgId common.Pgid
		if ref.node != nil {
			pgId = ref.node.inodes[ref.index].Pgid()
		} else {
			pgId = ref.page.BranchPageElement(uint16(ref.index)).Pgid()
		}
		p, n := c.bucket.pageNode(pgId)

		var nextRef = elemRef{page: p, node: n}
		nextRef.index = nextRef.count() - 1
		c.stack = append(c.stack, nextRef)
	}
}

// next moves to the next leaf element and returns the key and value.
// If the cursor is at the last leaf element then it stays there and returns nil.
func (c *Cursor) next() (key []byte, value []byte, flags uint32) {
	for {
		// Attempt to move over one element until we're successful.
		// Move up the stack as we hit the end of each page in our stack.
		var i int
		for i = len(c.stack) - 1; i >= 0; i-- {
			elem := &c.stack[i]
			if elem.index < elem.count()-1 {
				elem.index++
				break
			}
		}

		// If we've hit the root page then stop and return. This will leave the
		// cursor on the last element of the last page.
		if i == -1 {
			return nil, nil, 0
		}

		// Otherwise start from where we left off in the stack and find the
		// first element of the first leaf page.
		c.stack = c.stack[:i+1]
		c.goToFirstElementOnTheStack()

		// If this is an empty page then restart and move back up the stack.
		// https://github.com/boltdb/bolt/issues/450
		if c.stack[len(c.stack)-1].count() == 0 {
			continue
		}

		return c.keyValue()
	}
}

// prev moves the cursor to the previous item in the bucket and returns its key and value.
// If the cursor is at the beginning of the bucket then a nil key and value are returned.
func (c *Cursor) prev() (key []byte, value []byte, flags uint32) {
	// Attempt to move back one element until we're successful.
	// Move up the stack as we hit the beginning of each page in our stack.
	for i := len(c.stack) - 1; i >= 0; i-- {
		elem := &c.stack[i]
		if elem.index > 0 {
			elem.index--
			break
		}
		// If we've hit the beginning, we should stop moving the cursor,
		// and stay at the first element, so that users can continue to
		// iterate over the elements in reverse direction by calling `Next`.
		// We should return nil in such case.
		// Refer to https://github.com/etcd-io/bbolt/issues/733
		if len(c.stack) == 1 {
			c.first()
			return nil, nil, 0
		}
		c.stack = c.stack[:i]
	}

	// If we've hit the end then return nil.
	if len(c.stack) == 0 {
		return nil, nil, 0
	}

	// Move down the stack to find the last element of the last leaf under this branch.
	c.last()
	return c.keyValue()
}

// search recursively performs a binary search against a given page/node until it finds a given key.
func (c *Cursor) search(key []byte, pgId common.Pgid) {
	p, n := c.bucket.pageNode(pgId)
	if p != nil && !p.IsBranchPage() && !p.IsLeafPage() {
		panic(fmt.Sprintf("invalid page type: %d: %x", p.Id(), p.Flags()))
	}
	e := elemRef{page: p, node: n}
	c.stack = append(c.stack, e)

	// If we're on a leaf page/node then find the specific node.
	if e.isLeaf() {
		c.nsearch(key)
		return
	}

	if n != nil {
		c.searchNode(key, n)
		return
	}
	c.searchPage(key, p)
}

func (c *Cursor) searchNode(key []byte, n *node) {
	var exact bool
	index := sort.Search(len(n.inodes), func(i int) bool {
		// TODO(benbjohnson): Optimize this range search. It's a bit hacky right now.
		// sort.Search() finds the lowest index where f() != -1 but we need the highest index.
		ret := bytes.Compare(n.inodes[i].Key(), key)
		if ret == 0 {
			exact = true
		}
		return ret != -1
	})
	if !exact && index > 0 {
		index--
	}
	c.stack[len(c.stack)-1].index = index

	// Recursively search to the next page.
	c.search(key, n.inodes[index].Pgid())
}

func (c *Cursor) searchPage(key []byte, p *common.Page) {
	// Binary search for the correct range.
	inodes := p.BranchPageElements()

	var exact bool
	index := sort.Search(int(p.Count()), func(i int) bool {
		// TODO(benbjohnson): Optimize this range search. It's a bit hacky right now.
		// sort.Search() finds the lowest index where f() != -1 but we need the highest index.
		ret := bytes.Compare(inodes[i].Key(), key)
		if ret == 0 {
			exact = true
		}
		return ret != -1
	})
	if !exact && index > 0 {
		index--
	}
	c.stack[len(c.stack)-1].index = index

	// Recursively search to the next page.
	c.search(key, inodes[index].Pgid())
}

// nsearch searches the leaf node on the top of the stack for a key.
func (c *Cursor) nsearch(key []byte) {
	e := &c.stack[len(c.stack)-1]
	p, n := e.page, e.node

	// If we have a node then search its inodes.
	if n != nil {
		index := sort.Search(len(n.inodes), func(i int) bool {
			return bytes.Compare(n.inodes[i].Key(), key) != -1
		})
		e.index = index
		return
	}

	// If we have a page then search its leaf elements.
	inodes := p.LeafPageElements()
	index := sort.Search(int(p.Count()), func(i int) bool {
		return bytes.Compare(inodes[i].Key(), key) != -1
	})
	e.index = index
}

// keyValue returns the key and value of the current leaf element.
func (c *Cursor) keyValue() ([]byte, []byte, uint32) {
	ref := &c.stack[len(c.stack)-1]

	// If the cursor is pointing to the end of page/node then return nil.
	if ref.count() == 0 || ref.index >= ref.count() {
		return nil, nil, 0
	}

	// Retrieve value from node.
	if ref.node != nil {
		inode := &ref.node.inodes[ref.index]
		return inode.Key(), inode.Value(), inode.Flags()
	}

	// Or retrieve value from page.
	elem := ref.page.LeafPageElement(uint16(ref.index))
	return elem.Key(), elem.Value(), elem.Flags()
}

// node returns the node that the cursor is currently positioned on.
func (c *Cursor) node() *node {
	common.Assert(len(c.stack) > 0, "accessing a node with a zero-length cursor stack")

	// If the top of the stack is a leaf node then just return it.
	if ref := &c.stack[len(c.stack)-1]; ref.node != nil && ref.isLeaf() {
		return ref.node
	}

	// Start from root and traverse down the hierarchy.
	var n = c.stack[0].node
	if n == nil {
		n = c.bucket.node(c.stack[0].page.Id(), nil)
	}
	for _, ref := range c.stack[:len(c.stack)-1] {
		common.Assert(!n.isLeaf, "expected branch node")
		n = n.childAt(ref.index)
	}
	common.Assert(n.isLeaf, "expected leaf node")
	return n
}

// elemRef represents a reference to an element on a given page/node.
type elemRef struct {
	page  *common.Page
	node  *node
	index int
}

// isLeaf returns whether the ref is pointing at a leaf page/node.
func (r *elemRef) isLeaf() bool {
	if r.node != nil {
		return r.node.isLeaf
	}
	return r.page.IsLeafPage()
}

// count returns the number of inodes or page elements.
func (r *elemRef) count() int {
	if r.node != nil {
		return len(r.node.inodes)
	}
	return int(r.page.Count())
}
