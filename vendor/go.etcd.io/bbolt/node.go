package bbolt

import (
	"bytes"
	"fmt"
	"sort"

	"go.etcd.io/bbolt/internal/common"
)

// node represents an in-memory, deserialized page.
type node struct {
	bucket     *Bucket
	isLeaf     bool
	unbalanced bool
	spilled    bool
	key        []byte
	pgid       common.Pgid
	parent     *node
	children   nodes
	inodes     common.Inodes
}

// root returns the top-level node this node is attached to.
func (n *node) root() *node {
	if n.parent == nil {
		return n
	}
	return n.parent.root()
}

// minKeys returns the minimum number of inodes this node should have.
func (n *node) minKeys() int {
	if n.isLeaf {
		return 1
	}
	return 2
}

// size returns the size of the node after serialization.
func (n *node) size() int {
	sz, elsz := common.PageHeaderSize, n.pageElementSize()
	for i := 0; i < len(n.inodes); i++ {
		item := &n.inodes[i]
		sz += elsz + uintptr(len(item.Key())) + uintptr(len(item.Value()))
	}
	return int(sz)
}

// sizeLessThan returns true if the node is less than a given size.
// This is an optimization to avoid calculating a large node when we only need
// to know if it fits inside a certain page size.
func (n *node) sizeLessThan(v uintptr) bool {
	sz, elsz := common.PageHeaderSize, n.pageElementSize()
	for i := 0; i < len(n.inodes); i++ {
		item := &n.inodes[i]
		sz += elsz + uintptr(len(item.Key())) + uintptr(len(item.Value()))
		if sz >= v {
			return false
		}
	}
	return true
}

// pageElementSize returns the size of each page element based on the type of node.
func (n *node) pageElementSize() uintptr {
	if n.isLeaf {
		return common.LeafPageElementSize
	}
	return common.BranchPageElementSize
}

// childAt returns the child node at a given index.
func (n *node) childAt(index int) *node {
	if n.isLeaf {
		panic(fmt.Sprintf("invalid childAt(%d) on a leaf node", index))
	}
	return n.bucket.node(n.inodes[index].Pgid(), n)
}

// childIndex returns the index of a given child node.
func (n *node) childIndex(child *node) int {
	index := sort.Search(len(n.inodes), func(i int) bool { return bytes.Compare(n.inodes[i].Key(), child.key) != -1 })
	return index
}

// numChildren returns the number of children.
func (n *node) numChildren() int {
	return len(n.inodes)
}

// nextSibling returns the next node with the same parent.
func (n *node) nextSibling() *node {
	if n.parent == nil {
		return nil
	}
	index := n.parent.childIndex(n)
	if index >= n.parent.numChildren()-1 {
		return nil
	}
	return n.parent.childAt(index + 1)
}

// prevSibling returns the previous node with the same parent.
func (n *node) prevSibling() *node {
	if n.parent == nil {
		return nil
	}
	index := n.parent.childIndex(n)
	if index == 0 {
		return nil
	}
	return n.parent.childAt(index - 1)
}

// put inserts a key/value.
func (n *node) put(oldKey, newKey, value []byte, pgId common.Pgid, flags uint32) {
	if pgId >= n.bucket.tx.meta.Pgid() {
		panic(fmt.Sprintf("pgId (%d) above high water mark (%d)", pgId, n.bucket.tx.meta.Pgid()))
	} else if len(oldKey) <= 0 {
		panic("put: zero-length old key")
	} else if len(newKey) <= 0 {
		panic("put: zero-length new key")
	}

	// Find insertion index.
	index := sort.Search(len(n.inodes), func(i int) bool { return bytes.Compare(n.inodes[i].Key(), oldKey) != -1 })

	// Add capacity and shift nodes if we don't have an exact match and need to insert.
	exact := len(n.inodes) > 0 && index < len(n.inodes) && bytes.Equal(n.inodes[index].Key(), oldKey)
	if !exact {
		n.inodes = append(n.inodes, common.Inode{})
		copy(n.inodes[index+1:], n.inodes[index:])
	}

	inode := &n.inodes[index]
	inode.SetFlags(flags)
	inode.SetKey(newKey)
	inode.SetValue(value)
	inode.SetPgid(pgId)
	common.Assert(len(inode.Key()) > 0, "put: zero-length inode key")
}

// del removes a key from the node.
func (n *node) del(key []byte) {
	// Find index of key.
	index := sort.Search(len(n.inodes), func(i int) bool { return bytes.Compare(n.inodes[i].Key(), key) != -1 })

	// Exit if the key isn't found.
	if index >= len(n.inodes) || !bytes.Equal(n.inodes[index].Key(), key) {
		return
	}

	// Delete inode from the node.
	n.inodes = append(n.inodes[:index], n.inodes[index+1:]...)

	// Mark the node as needing rebalancing.
	n.unbalanced = true
}

// read initializes the node from a page.
func (n *node) read(p *common.Page) {
	n.pgid = p.Id()
	n.isLeaf = p.IsLeafPage()
	n.inodes = common.ReadInodeFromPage(p)

	// Save first key, so we can find the node in the parent when we spill.
	if len(n.inodes) > 0 {
		n.key = n.inodes[0].Key()
		common.Assert(len(n.key) > 0, "read: zero-length node key")
	} else {
		n.key = nil
	}
}

// write writes the items onto one or more pages.
// The page should have p.id (might be 0 for meta or bucket-inline page) and p.overflow set
// and the rest should be zeroed.
func (n *node) write(p *common.Page) {
	common.Assert(p.Count() == 0 && p.Flags() == 0, "node cannot be written into a not empty page")

	// Initialize page.
	if n.isLeaf {
		p.SetFlags(common.LeafPageFlag)
	} else {
		p.SetFlags(common.BranchPageFlag)
	}

	if len(n.inodes) >= 0xFFFF {
		panic(fmt.Sprintf("inode overflow: %d (pgid=%d)", len(n.inodes), p.Id()))
	}
	p.SetCount(uint16(len(n.inodes)))

	// Stop here if there are no items to write.
	if p.Count() == 0 {
		return
	}

	common.WriteInodeToPage(n.inodes, p)

	// DEBUG ONLY: n.dump()
}

// split breaks up a node into multiple smaller nodes, if appropriate.
// This should only be called from the spill() function.
func (n *node) split(pageSize uintptr) []*node {
	var nodes []*node

	node := n
	for {
		// Split node into two.
		a, b := node.splitTwo(pageSize)
		nodes = append(nodes, a)

		// If we can't split then exit the loop.
		if b == nil {
			break
		}

		// Set node to b so it gets split on the next iteration.
		node = b
	}

	return nodes
}

// splitTwo breaks up a node into two smaller nodes, if appropriate.
// This should only be called from the split() function.
func (n *node) splitTwo(pageSize uintptr) (*node, *node) {
	// Ignore the split if the page doesn't have at least enough nodes for
	// two pages or if the nodes can fit in a single page.
	if len(n.inodes) <= (common.MinKeysPerPage*2) || n.sizeLessThan(pageSize) {
		return n, nil
	}

	// Determine the threshold before starting a new node.
	var fillPercent = n.bucket.FillPercent
	if fillPercent < minFillPercent {
		fillPercent = minFillPercent
	} else if fillPercent > maxFillPercent {
		fillPercent = maxFillPercent
	}
	threshold := int(float64(pageSize) * fillPercent)

	// Determine split position and sizes of the two pages.
	splitIndex, _ := n.splitIndex(threshold)

	// Split node into two separate nodes.
	// If there's no parent then we'll need to create one.
	if n.parent == nil {
		n.parent = &node{bucket: n.bucket, children: []*node{n}}
	}

	// Create a new node and add it to the parent.
	next := &node{bucket: n.bucket, isLeaf: n.isLeaf, parent: n.parent}
	n.parent.children = append(n.parent.children, next)

	// Split inodes across two nodes.
	next.inodes = n.inodes[splitIndex:]
	n.inodes = n.inodes[:splitIndex]

	// Update the statistics.
	n.bucket.tx.stats.IncSplit(1)

	return n, next
}

// splitIndex finds the position where a page will fill a given threshold.
// It returns the index as well as the size of the first page.
// This is only be called from split().
func (n *node) splitIndex(threshold int) (index, sz uintptr) {
	sz = common.PageHeaderSize

	// Loop until we only have the minimum number of keys required for the second page.
	for i := 0; i < len(n.inodes)-common.MinKeysPerPage; i++ {
		index = uintptr(i)
		inode := n.inodes[i]
		elsize := n.pageElementSize() + uintptr(len(inode.Key())) + uintptr(len(inode.Value()))

		// If we have at least the minimum number of keys and adding another
		// node would put us over the threshold then exit and return.
		if index >= common.MinKeysPerPage && sz+elsize > uintptr(threshold) {
			break
		}

		// Add the element size to the total size.
		sz += elsize
	}

	return
}

// spill writes the nodes to dirty pages and splits nodes as it goes.
// Returns an error if dirty pages cannot be allocated.
func (n *node) spill() error {
	var tx = n.bucket.tx
	if n.spilled {
		return nil
	}

	// Spill child nodes first. Child nodes can materialize sibling nodes in
	// the case of split-merge so we cannot use a range loop. We have to check
	// the children size on every loop iteration.
	sort.Sort(n.children)
	for i := 0; i < len(n.children); i++ {
		if err := n.children[i].spill(); err != nil {
			return err
		}
	}

	// We no longer need the child list because it's only used for spill tracking.
	n.children = nil

	// Split nodes into appropriate sizes. The first node will always be n.
	var nodes = n.split(uintptr(tx.db.pageSize))
	for _, node := range nodes {
		// Add node's page to the freelist if it's not new.
		if node.pgid > 0 {
			tx.db.freelist.Free(tx.meta.Txid(), tx.page(node.pgid))
			node.pgid = 0
		}

		// Allocate contiguous space for the node.
		p, err := tx.allocate((node.size() + tx.db.pageSize - 1) / tx.db.pageSize)
		if err != nil {
			return err
		}

		// Write the node.
		if p.Id() >= tx.meta.Pgid() {
			panic(fmt.Sprintf("pgid (%d) above high water mark (%d)", p.Id(), tx.meta.Pgid()))
		}
		node.pgid = p.Id()
		node.write(p)
		node.spilled = true

		// Insert into parent inodes.
		if node.parent != nil {
			var key = node.key
			if key == nil {
				key = node.inodes[0].Key()
			}

			node.parent.put(key, node.inodes[0].Key(), nil, node.pgid, 0)
			node.key = node.inodes[0].Key()
			common.Assert(len(node.key) > 0, "spill: zero-length node key")
		}

		// Update the statistics.
		tx.stats.IncSpill(1)
	}

	// If the root node split and created a new root then we need to spill that
	// as well. We'll clear out the children to make sure it doesn't try to respill.
	if n.parent != nil && n.parent.pgid == 0 {
		n.children = nil
		return n.parent.spill()
	}

	return nil
}

// rebalance attempts to combine the node with sibling nodes if the node fill
// size is below a threshold or if there are not enough keys.
func (n *node) rebalance() {
	if !n.unbalanced {
		return
	}
	n.unbalanced = false

	// Update statistics.
	n.bucket.tx.stats.IncRebalance(1)

	// Ignore if node is above threshold (25% when FillPercent is set to DefaultFillPercent) and has enough keys.
	var threshold = int(float64(n.bucket.tx.db.pageSize)*n.bucket.FillPercent) / 2
	if n.size() > threshold && len(n.inodes) > n.minKeys() {
		return
	}

	// Root node has special handling.
	if n.parent == nil {
		// If root node is a branch and only has one node then collapse it.
		if !n.isLeaf && len(n.inodes) == 1 {
			// Move root's child up.
			child := n.bucket.node(n.inodes[0].Pgid(), n)
			n.isLeaf = child.isLeaf
			n.inodes = child.inodes[:]
			n.children = child.children

			// Reparent all child nodes being moved.
			for _, inode := range n.inodes {
				if child, ok := n.bucket.nodes[inode.Pgid()]; ok {
					child.parent = n
				}
			}

			// Remove old child.
			child.parent = nil
			delete(n.bucket.nodes, child.pgid)
			child.free()
		}

		return
	}

	// If node has no keys then just remove it.
	if n.numChildren() == 0 {
		n.parent.del(n.key)
		n.parent.removeChild(n)
		delete(n.bucket.nodes, n.pgid)
		n.free()
		n.parent.rebalance()
		return
	}

	common.Assert(n.parent.numChildren() > 1, "parent must have at least 2 children")

	// Merge with right sibling if idx == 0, otherwise left sibling.
	var leftNode, rightNode *node
	var useNextSibling = n.parent.childIndex(n) == 0
	if useNextSibling {
		leftNode = n
		rightNode = n.nextSibling()
	} else {
		leftNode = n.prevSibling()
		rightNode = n
	}

	// If both nodes are too small then merge them.
	// Reparent all child nodes being moved.
	for _, inode := range rightNode.inodes {
		if child, ok := n.bucket.nodes[inode.Pgid()]; ok {
			child.parent.removeChild(child)
			child.parent = leftNode
			child.parent.children = append(child.parent.children, child)
		}
	}

	// Copy over inodes from right node to left node and remove right node.
	leftNode.inodes = append(leftNode.inodes, rightNode.inodes...)
	n.parent.del(rightNode.key)
	n.parent.removeChild(rightNode)
	delete(n.bucket.nodes, rightNode.pgid)
	rightNode.free()

	// Either this node or the sibling node was deleted from the parent so rebalance it.
	n.parent.rebalance()
}

// removes a node from the list of in-memory children.
// This does not affect the inodes.
func (n *node) removeChild(target *node) {
	for i, child := range n.children {
		if child == target {
			n.children = append(n.children[:i], n.children[i+1:]...)
			return
		}
	}
}

// dereference causes the node to copy all its inode key/value references to heap memory.
// This is required when the mmap is reallocated so inodes are not pointing to stale data.
func (n *node) dereference() {
	if n.key != nil {
		key := make([]byte, len(n.key))
		copy(key, n.key)
		n.key = key
		common.Assert(n.pgid == 0 || len(n.key) > 0, "dereference: zero-length node key on existing node")
	}

	for i := range n.inodes {
		inode := &n.inodes[i]

		key := make([]byte, len(inode.Key()))
		copy(key, inode.Key())
		inode.SetKey(key)
		common.Assert(len(inode.Key()) > 0, "dereference: zero-length inode key")

		value := make([]byte, len(inode.Value()))
		copy(value, inode.Value())
		inode.SetValue(value)
	}

	// Recursively dereference children.
	for _, child := range n.children {
		child.dereference()
	}

	// Update statistics.
	n.bucket.tx.stats.IncNodeDeref(1)
}

// free adds the node's underlying page to the freelist.
func (n *node) free() {
	if n.pgid != 0 {
		n.bucket.tx.db.freelist.Free(n.bucket.tx.meta.Txid(), n.bucket.tx.page(n.pgid))
		n.pgid = 0
	}
}

// dump writes the contents of the node to STDERR for debugging purposes.
/*
func (n *node) dump() {
	// Write node header.
	var typ = "branch"
	if n.isLeaf {
		typ = "leaf"
	}
	warnf("[NODE %d {type=%s count=%d}]", n.pgid, typ, len(n.inodes))

	// Write out abbreviated version of each item.
	for _, item := range n.inodes {
		if n.isLeaf {
			if item.flags&bucketLeafFlag != 0 {
				bucket := (*bucket)(unsafe.Pointer(&item.value[0]))
				warnf("+L %08x -> (bucket root=%d)", trunc(item.key, 4), bucket.root)
			} else {
				warnf("+L %08x -> %08x", trunc(item.key, 4), trunc(item.value, 4))
			}
		} else {
			warnf("+B %08x -> pgid=%d", trunc(item.key, 4), item.pgid)
		}
	}
	warn("")
}
*/

func compareKeys(left, right []byte) int {
	return bytes.Compare(left, right)
}

type nodes []*node

func (s nodes) Len() int      { return len(s) }
func (s nodes) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func (s nodes) Less(i, j int) bool {
	return bytes.Compare(s[i].inodes[0].Key(), s[j].inodes[0].Key()) == -1
}
