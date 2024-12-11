package bbolt

import (
	"bytes"
	"fmt"
	"unsafe"

	"go.etcd.io/bbolt/errors"
	"go.etcd.io/bbolt/internal/common"
)

const (
	// MaxKeySize is the maximum length of a key, in bytes.
	MaxKeySize = 32768

	// MaxValueSize is the maximum length of a value, in bytes.
	MaxValueSize = (1 << 31) - 2
)

const (
	minFillPercent = 0.1
	maxFillPercent = 1.0
)

// DefaultFillPercent is the percentage that split pages are filled.
// This value can be changed by setting Bucket.FillPercent.
const DefaultFillPercent = 0.5

// Bucket represents a collection of key/value pairs inside the database.
type Bucket struct {
	*common.InBucket
	tx       *Tx                   // the associated transaction
	buckets  map[string]*Bucket    // subbucket cache
	page     *common.Page          // inline page reference
	rootNode *node                 // materialized node for the root page.
	nodes    map[common.Pgid]*node // node cache

	// Sets the threshold for filling nodes when they split. By default,
	// the bucket will fill to 50% but it can be useful to increase this
	// amount if you know that your write workloads are mostly append-only.
	//
	// This is non-persisted across transactions so it must be set in every Tx.
	FillPercent float64
}

// newBucket returns a new bucket associated with a transaction.
func newBucket(tx *Tx) Bucket {
	var b = Bucket{tx: tx, FillPercent: DefaultFillPercent}
	if tx.writable {
		b.buckets = make(map[string]*Bucket)
		b.nodes = make(map[common.Pgid]*node)
	}
	return b
}

// Tx returns the tx of the bucket.
func (b *Bucket) Tx() *Tx {
	return b.tx
}

// Root returns the root of the bucket.
func (b *Bucket) Root() common.Pgid {
	return b.RootPage()
}

// Writable returns whether the bucket is writable.
func (b *Bucket) Writable() bool {
	return b.tx.writable
}

// Cursor creates a cursor associated with the bucket.
// The cursor is only valid as long as the transaction is open.
// Do not use a cursor after the transaction is closed.
func (b *Bucket) Cursor() *Cursor {
	// Update transaction statistics.
	b.tx.stats.IncCursorCount(1)

	// Allocate and return a cursor.
	return &Cursor{
		bucket: b,
		stack:  make([]elemRef, 0),
	}
}

// Bucket retrieves a nested bucket by name.
// Returns nil if the bucket does not exist.
// The bucket instance is only valid for the lifetime of the transaction.
func (b *Bucket) Bucket(name []byte) *Bucket {
	if b.buckets != nil {
		if child := b.buckets[string(name)]; child != nil {
			return child
		}
	}

	// Move cursor to key.
	c := b.Cursor()
	k, v, flags := c.seek(name)

	// Return nil if the key doesn't exist or it is not a bucket.
	if !bytes.Equal(name, k) || (flags&common.BucketLeafFlag) == 0 {
		return nil
	}

	// Otherwise create a bucket and cache it.
	var child = b.openBucket(v)
	if b.buckets != nil {
		b.buckets[string(name)] = child
	}

	return child
}

// Helper method that re-interprets a sub-bucket value
// from a parent into a Bucket
func (b *Bucket) openBucket(value []byte) *Bucket {
	var child = newBucket(b.tx)

	// Unaligned access requires a copy to be made.
	const unalignedMask = unsafe.Alignof(struct {
		common.InBucket
		common.Page
	}{}) - 1
	unaligned := uintptr(unsafe.Pointer(&value[0]))&unalignedMask != 0
	if unaligned {
		value = cloneBytes(value)
	}

	// If this is a writable transaction then we need to copy the bucket entry.
	// Read-only transactions can point directly at the mmap entry.
	if b.tx.writable && !unaligned {
		child.InBucket = &common.InBucket{}
		*child.InBucket = *(*common.InBucket)(unsafe.Pointer(&value[0]))
	} else {
		child.InBucket = (*common.InBucket)(unsafe.Pointer(&value[0]))
	}

	// Save a reference to the inline page if the bucket is inline.
	if child.RootPage() == 0 {
		child.page = (*common.Page)(unsafe.Pointer(&value[common.BucketHeaderSize]))
	}

	return &child
}

// CreateBucket creates a new bucket at the given key and returns the new bucket.
// Returns an error if the key already exists, if the bucket name is blank, or if the bucket name is too long.
// The bucket instance is only valid for the lifetime of the transaction.
func (b *Bucket) CreateBucket(key []byte) (rb *Bucket, err error) {
	if lg := b.tx.db.Logger(); lg != discardLogger {
		lg.Debugf("Creating bucket %q", key)
		defer func() {
			if err != nil {
				lg.Errorf("Creating bucket %q failed: %v", key, err)
			} else {
				lg.Debugf("Creating bucket %q successfully", key)
			}
		}()
	}
	if b.tx.db == nil {
		return nil, errors.ErrTxClosed
	} else if !b.tx.writable {
		return nil, errors.ErrTxNotWritable
	} else if len(key) == 0 {
		return nil, errors.ErrBucketNameRequired
	}

	// Insert into node.
	// Tip: Use a new variable `newKey` instead of reusing the existing `key` to prevent
	// it from being marked as leaking, and accordingly cannot be allocated on stack.
	newKey := cloneBytes(key)

	// Move cursor to correct position.
	c := b.Cursor()
	k, _, flags := c.seek(newKey)

	// Return an error if there is an existing key.
	if bytes.Equal(newKey, k) {
		if (flags & common.BucketLeafFlag) != 0 {
			return nil, errors.ErrBucketExists
		}
		return nil, errors.ErrIncompatibleValue
	}

	// Create empty, inline bucket.
	var bucket = Bucket{
		InBucket:    &common.InBucket{},
		rootNode:    &node{isLeaf: true},
		FillPercent: DefaultFillPercent,
	}
	var value = bucket.write()

	c.node().put(newKey, newKey, value, 0, common.BucketLeafFlag)

	// Since subbuckets are not allowed on inline buckets, we need to
	// dereference the inline page, if it exists. This will cause the bucket
	// to be treated as a regular, non-inline bucket for the rest of the tx.
	b.page = nil

	return b.Bucket(newKey), nil
}

// CreateBucketIfNotExists creates a new bucket if it doesn't already exist and returns a reference to it.
// Returns an error if the bucket name is blank, or if the bucket name is too long.
// The bucket instance is only valid for the lifetime of the transaction.
func (b *Bucket) CreateBucketIfNotExists(key []byte) (rb *Bucket, err error) {
	if lg := b.tx.db.Logger(); lg != discardLogger {
		lg.Debugf("Creating bucket if not exist %q", key)
		defer func() {
			if err != nil {
				lg.Errorf("Creating bucket if not exist %q failed: %v", key, err)
			} else {
				lg.Debugf("Creating bucket if not exist %q successfully", key)
			}
		}()
	}

	if b.tx.db == nil {
		return nil, errors.ErrTxClosed
	} else if !b.tx.writable {
		return nil, errors.ErrTxNotWritable
	} else if len(key) == 0 {
		return nil, errors.ErrBucketNameRequired
	}

	// Insert into node.
	// Tip: Use a new variable `newKey` instead of reusing the existing `key` to prevent
	// it from being marked as leaking, and accordingly cannot be allocated on stack.
	newKey := cloneBytes(key)

	if b.buckets != nil {
		if child := b.buckets[string(newKey)]; child != nil {
			return child, nil
		}
	}

	// Move cursor to correct position.
	c := b.Cursor()
	k, v, flags := c.seek(newKey)

	// Return an error if there is an existing non-bucket key.
	if bytes.Equal(newKey, k) {
		if (flags & common.BucketLeafFlag) != 0 {
			var child = b.openBucket(v)
			if b.buckets != nil {
				b.buckets[string(newKey)] = child
			}

			return child, nil
		}
		return nil, errors.ErrIncompatibleValue
	}

	// Create empty, inline bucket.
	var bucket = Bucket{
		InBucket:    &common.InBucket{},
		rootNode:    &node{isLeaf: true},
		FillPercent: DefaultFillPercent,
	}
	var value = bucket.write()

	c.node().put(newKey, newKey, value, 0, common.BucketLeafFlag)

	// Since subbuckets are not allowed on inline buckets, we need to
	// dereference the inline page, if it exists. This will cause the bucket
	// to be treated as a regular, non-inline bucket for the rest of the tx.
	b.page = nil

	return b.Bucket(newKey), nil
}

// DeleteBucket deletes a bucket at the given key.
// Returns an error if the bucket does not exist, or if the key represents a non-bucket value.
func (b *Bucket) DeleteBucket(key []byte) (err error) {
	if lg := b.tx.db.Logger(); lg != discardLogger {
		lg.Debugf("Deleting bucket %q", key)
		defer func() {
			if err != nil {
				lg.Errorf("Deleting bucket %q failed: %v", key, err)
			} else {
				lg.Debugf("Deleting bucket %q successfully", key)
			}
		}()
	}

	if b.tx.db == nil {
		return errors.ErrTxClosed
	} else if !b.Writable() {
		return errors.ErrTxNotWritable
	}

	newKey := cloneBytes(key)

	// Move cursor to correct position.
	c := b.Cursor()
	k, _, flags := c.seek(newKey)

	// Return an error if bucket doesn't exist or is not a bucket.
	if !bytes.Equal(newKey, k) {
		return errors.ErrBucketNotFound
	} else if (flags & common.BucketLeafFlag) == 0 {
		return errors.ErrIncompatibleValue
	}

	// Recursively delete all child buckets.
	child := b.Bucket(newKey)
	err = child.ForEachBucket(func(k []byte) error {
		if err := child.DeleteBucket(k); err != nil {
			return fmt.Errorf("delete bucket: %s", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Remove cached copy.
	delete(b.buckets, string(newKey))

	// Release all bucket pages to freelist.
	child.nodes = nil
	child.rootNode = nil
	child.free()

	// Delete the node if we have a matching key.
	c.node().del(newKey)

	return nil
}

// MoveBucket moves a sub-bucket from the source bucket to the destination bucket.
// Returns an error if
//  1. the sub-bucket cannot be found in the source bucket;
//  2. or the key already exists in the destination bucket;
//  3. or the key represents a non-bucket value;
//  4. the source and destination buckets are the same.
func (b *Bucket) MoveBucket(key []byte, dstBucket *Bucket) (err error) {
	lg := b.tx.db.Logger()
	if lg != discardLogger {
		lg.Debugf("Moving bucket %q", key)
		defer func() {
			if err != nil {
				lg.Errorf("Moving bucket %q failed: %v", key, err)
			} else {
				lg.Debugf("Moving bucket %q successfully", key)
			}
		}()
	}

	if b.tx.db == nil || dstBucket.tx.db == nil {
		return errors.ErrTxClosed
	} else if !b.Writable() || !dstBucket.Writable() {
		return errors.ErrTxNotWritable
	}

	if b.tx.db.Path() != dstBucket.tx.db.Path() || b.tx != dstBucket.tx {
		lg.Errorf("The source and target buckets are not in the same db file, source bucket in %s and target bucket in %s", b.tx.db.Path(), dstBucket.tx.db.Path())
		return errors.ErrDifferentDB
	}

	newKey := cloneBytes(key)

	// Move cursor to correct position.
	c := b.Cursor()
	k, v, flags := c.seek(newKey)

	// Return an error if bucket doesn't exist or is not a bucket.
	if !bytes.Equal(newKey, k) {
		return errors.ErrBucketNotFound
	} else if (flags & common.BucketLeafFlag) == 0 {
		lg.Errorf("An incompatible key %s exists in the source bucket", newKey)
		return errors.ErrIncompatibleValue
	}

	// Do nothing (return true directly) if the source bucket and the
	// destination bucket are actually the same bucket.
	if b == dstBucket || (b.RootPage() == dstBucket.RootPage() && b.RootPage() != 0) {
		lg.Errorf("The source bucket (%s) and the target bucket (%s) are the same bucket", b, dstBucket)
		return errors.ErrSameBuckets
	}

	// check whether the key already exists in the destination bucket
	curDst := dstBucket.Cursor()
	k, _, flags = curDst.seek(newKey)

	// Return an error if there is an existing key in the destination bucket.
	if bytes.Equal(newKey, k) {
		if (flags & common.BucketLeafFlag) != 0 {
			return errors.ErrBucketExists
		}
		lg.Errorf("An incompatible key %s exists in the target bucket", newKey)
		return errors.ErrIncompatibleValue
	}

	// remove the sub-bucket from the source bucket
	delete(b.buckets, string(newKey))
	c.node().del(newKey)

	// add te sub-bucket to the destination bucket
	newValue := cloneBytes(v)
	curDst.node().put(newKey, newKey, newValue, 0, common.BucketLeafFlag)

	return nil
}

// Inspect returns the structure of the bucket.
func (b *Bucket) Inspect() BucketStructure {
	return b.recursivelyInspect([]byte("root"))
}

func (b *Bucket) recursivelyInspect(name []byte) BucketStructure {
	bs := BucketStructure{Name: string(name)}

	keyN := 0
	c := b.Cursor()
	for k, _, flags := c.first(); k != nil; k, _, flags = c.next() {
		if flags&common.BucketLeafFlag != 0 {
			childBucket := b.Bucket(k)
			childBS := childBucket.recursivelyInspect(k)
			bs.Children = append(bs.Children, childBS)
		} else {
			keyN++
		}
	}
	bs.KeyN = keyN

	return bs
}

// Get retrieves the value for a key in the bucket.
// Returns a nil value if the key does not exist or if the key is a nested bucket.
// The returned value is only valid for the life of the transaction.
// The returned memory is owned by bbolt and must never be modified; writing to this memory might corrupt the database.
func (b *Bucket) Get(key []byte) []byte {
	k, v, flags := b.Cursor().seek(key)

	// Return nil if this is a bucket.
	if (flags & common.BucketLeafFlag) != 0 {
		return nil
	}

	// If our target node isn't the same key as what's passed in then return nil.
	if !bytes.Equal(key, k) {
		return nil
	}
	return v
}

// Put sets the value for a key in the bucket.
// If the key exist then its previous value will be overwritten.
// Supplied value must remain valid for the life of the transaction.
// Returns an error if the bucket was created from a read-only transaction, if the key is blank, if the key is too large, or if the value is too large.
func (b *Bucket) Put(key []byte, value []byte) (err error) {
	if lg := b.tx.db.Logger(); lg != discardLogger {
		lg.Debugf("Putting key %q", key)
		defer func() {
			if err != nil {
				lg.Errorf("Putting key %q failed: %v", key, err)
			} else {
				lg.Debugf("Putting key %q successfully", key)
			}
		}()
	}
	if b.tx.db == nil {
		return errors.ErrTxClosed
	} else if !b.Writable() {
		return errors.ErrTxNotWritable
	} else if len(key) == 0 {
		return errors.ErrKeyRequired
	} else if len(key) > MaxKeySize {
		return errors.ErrKeyTooLarge
	} else if int64(len(value)) > MaxValueSize {
		return errors.ErrValueTooLarge
	}

	// Insert into node.
	// Tip: Use a new variable `newKey` instead of reusing the existing `key` to prevent
	// it from being marked as leaking, and accordingly cannot be allocated on stack.
	newKey := cloneBytes(key)

	// Move cursor to correct position.
	c := b.Cursor()
	k, _, flags := c.seek(newKey)

	// Return an error if there is an existing key with a bucket value.
	if bytes.Equal(newKey, k) && (flags&common.BucketLeafFlag) != 0 {
		return errors.ErrIncompatibleValue
	}

	// gofail: var beforeBucketPut struct{}

	c.node().put(newKey, newKey, value, 0, 0)

	return nil
}

// Delete removes a key from the bucket.
// If the key does not exist then nothing is done and a nil error is returned.
// Returns an error if the bucket was created from a read-only transaction.
func (b *Bucket) Delete(key []byte) (err error) {
	if lg := b.tx.db.Logger(); lg != discardLogger {
		lg.Debugf("Deleting key %q", key)
		defer func() {
			if err != nil {
				lg.Errorf("Deleting key %q failed: %v", key, err)
			} else {
				lg.Debugf("Deleting key %q successfully", key)
			}
		}()
	}

	if b.tx.db == nil {
		return errors.ErrTxClosed
	} else if !b.Writable() {
		return errors.ErrTxNotWritable
	}

	// Move cursor to correct position.
	c := b.Cursor()
	k, _, flags := c.seek(key)

	// Return nil if the key doesn't exist.
	if !bytes.Equal(key, k) {
		return nil
	}

	// Return an error if there is already existing bucket value.
	if (flags & common.BucketLeafFlag) != 0 {
		return errors.ErrIncompatibleValue
	}

	// Delete the node if we have a matching key.
	c.node().del(key)

	return nil
}

// Sequence returns the current integer for the bucket without incrementing it.
func (b *Bucket) Sequence() uint64 {
	return b.InSequence()
}

// SetSequence updates the sequence number for the bucket.
func (b *Bucket) SetSequence(v uint64) error {
	if b.tx.db == nil {
		return errors.ErrTxClosed
	} else if !b.Writable() {
		return errors.ErrTxNotWritable
	}

	// Materialize the root node if it hasn't been already so that the
	// bucket will be saved during commit.
	if b.rootNode == nil {
		_ = b.node(b.RootPage(), nil)
	}

	// Set the sequence.
	b.SetInSequence(v)
	return nil
}

// NextSequence returns an autoincrementing integer for the bucket.
func (b *Bucket) NextSequence() (uint64, error) {
	if b.tx.db == nil {
		return 0, errors.ErrTxClosed
	} else if !b.Writable() {
		return 0, errors.ErrTxNotWritable
	}

	// Materialize the root node if it hasn't been already so that the
	// bucket will be saved during commit.
	if b.rootNode == nil {
		_ = b.node(b.RootPage(), nil)
	}

	// Increment and return the sequence.
	b.IncSequence()
	return b.Sequence(), nil
}

// ForEach executes a function for each key/value pair in a bucket.
// Because ForEach uses a Cursor, the iteration over keys is in lexicographical order.
// If the provided function returns an error then the iteration is stopped and
// the error is returned to the caller. The provided function must not modify
// the bucket; this will result in undefined behavior.
func (b *Bucket) ForEach(fn func(k, v []byte) error) error {
	if b.tx.db == nil {
		return errors.ErrTxClosed
	}
	c := b.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if err := fn(k, v); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bucket) ForEachBucket(fn func(k []byte) error) error {
	if b.tx.db == nil {
		return errors.ErrTxClosed
	}
	c := b.Cursor()
	for k, _, flags := c.first(); k != nil; k, _, flags = c.next() {
		if flags&common.BucketLeafFlag != 0 {
			if err := fn(k); err != nil {
				return err
			}
		}
	}
	return nil
}

// Stats returns stats on a bucket.
func (b *Bucket) Stats() BucketStats {
	var s, subStats BucketStats
	pageSize := b.tx.db.pageSize
	s.BucketN += 1
	if b.RootPage() == 0 {
		s.InlineBucketN += 1
	}
	b.forEachPage(func(p *common.Page, depth int, pgstack []common.Pgid) {
		if p.IsLeafPage() {
			s.KeyN += int(p.Count())

			// used totals the used bytes for the page
			used := common.PageHeaderSize

			if p.Count() != 0 {
				// If page has any elements, add all element headers.
				used += common.LeafPageElementSize * uintptr(p.Count()-1)

				// Add all element key, value sizes.
				// The computation takes advantage of the fact that the position
				// of the last element's key/value equals to the total of the sizes
				// of all previous elements' keys and values.
				// It also includes the last element's header.
				lastElement := p.LeafPageElement(p.Count() - 1)
				used += uintptr(lastElement.Pos() + lastElement.Ksize() + lastElement.Vsize())
			}

			if b.RootPage() == 0 {
				// For inlined bucket just update the inline stats
				s.InlineBucketInuse += int(used)
			} else {
				// For non-inlined bucket update all the leaf stats
				s.LeafPageN++
				s.LeafInuse += int(used)
				s.LeafOverflowN += int(p.Overflow())

				// Collect stats from sub-buckets.
				// Do that by iterating over all element headers
				// looking for the ones with the bucketLeafFlag.
				for i := uint16(0); i < p.Count(); i++ {
					e := p.LeafPageElement(i)
					if (e.Flags() & common.BucketLeafFlag) != 0 {
						// For any bucket element, open the element value
						// and recursively call Stats on the contained bucket.
						subStats.Add(b.openBucket(e.Value()).Stats())
					}
				}
			}
		} else if p.IsBranchPage() {
			s.BranchPageN++
			lastElement := p.BranchPageElement(p.Count() - 1)

			// used totals the used bytes for the page
			// Add header and all element headers.
			used := common.PageHeaderSize + (common.BranchPageElementSize * uintptr(p.Count()-1))

			// Add size of all keys and values.
			// Again, use the fact that last element's position equals to
			// the total of key, value sizes of all previous elements.
			used += uintptr(lastElement.Pos() + lastElement.Ksize())
			s.BranchInuse += int(used)
			s.BranchOverflowN += int(p.Overflow())
		}

		// Keep track of maximum page depth.
		if depth+1 > s.Depth {
			s.Depth = depth + 1
		}
	})

	// Alloc stats can be computed from page counts and pageSize.
	s.BranchAlloc = (s.BranchPageN + s.BranchOverflowN) * pageSize
	s.LeafAlloc = (s.LeafPageN + s.LeafOverflowN) * pageSize

	// Add the max depth of sub-buckets to get total nested depth.
	s.Depth += subStats.Depth
	// Add the stats for all sub-buckets
	s.Add(subStats)
	return s
}

// forEachPage iterates over every page in a bucket, including inline pages.
func (b *Bucket) forEachPage(fn func(*common.Page, int, []common.Pgid)) {
	// If we have an inline page then just use that.
	if b.page != nil {
		fn(b.page, 0, []common.Pgid{b.RootPage()})
		return
	}

	// Otherwise traverse the page hierarchy.
	b.tx.forEachPage(b.RootPage(), fn)
}

// forEachPageNode iterates over every page (or node) in a bucket.
// This also includes inline pages.
func (b *Bucket) forEachPageNode(fn func(*common.Page, *node, int)) {
	// If we have an inline page or root node then just use that.
	if b.page != nil {
		fn(b.page, nil, 0)
		return
	}
	b._forEachPageNode(b.RootPage(), 0, fn)
}

func (b *Bucket) _forEachPageNode(pgId common.Pgid, depth int, fn func(*common.Page, *node, int)) {
	var p, n = b.pageNode(pgId)

	// Execute function.
	fn(p, n, depth)

	// Recursively loop over children.
	if p != nil {
		if p.IsBranchPage() {
			for i := 0; i < int(p.Count()); i++ {
				elem := p.BranchPageElement(uint16(i))
				b._forEachPageNode(elem.Pgid(), depth+1, fn)
			}
		}
	} else {
		if !n.isLeaf {
			for _, inode := range n.inodes {
				b._forEachPageNode(inode.Pgid(), depth+1, fn)
			}
		}
	}
}

// spill writes all the nodes for this bucket to dirty pages.
func (b *Bucket) spill() error {
	// Spill all child buckets first.
	for name, child := range b.buckets {
		// If the child bucket is small enough and it has no child buckets then
		// write it inline into the parent bucket's page. Otherwise spill it
		// like a normal bucket and make the parent value a pointer to the page.
		var value []byte
		if child.inlineable() {
			child.free()
			value = child.write()
		} else {
			if err := child.spill(); err != nil {
				return err
			}

			// Update the child bucket header in this bucket.
			value = make([]byte, unsafe.Sizeof(common.InBucket{}))
			var bucket = (*common.InBucket)(unsafe.Pointer(&value[0]))
			*bucket = *child.InBucket
		}

		// Skip writing the bucket if there are no materialized nodes.
		if child.rootNode == nil {
			continue
		}

		// Update parent node.
		var c = b.Cursor()
		k, _, flags := c.seek([]byte(name))
		if !bytes.Equal([]byte(name), k) {
			panic(fmt.Sprintf("misplaced bucket header: %x -> %x", []byte(name), k))
		}
		if flags&common.BucketLeafFlag == 0 {
			panic(fmt.Sprintf("unexpected bucket header flag: %x", flags))
		}
		c.node().put([]byte(name), []byte(name), value, 0, common.BucketLeafFlag)
	}

	// Ignore if there's not a materialized root node.
	if b.rootNode == nil {
		return nil
	}

	// Spill nodes.
	if err := b.rootNode.spill(); err != nil {
		return err
	}
	b.rootNode = b.rootNode.root()

	// Update the root node for this bucket.
	if b.rootNode.pgid >= b.tx.meta.Pgid() {
		panic(fmt.Sprintf("pgid (%d) above high water mark (%d)", b.rootNode.pgid, b.tx.meta.Pgid()))
	}
	b.SetRootPage(b.rootNode.pgid)

	return nil
}

// inlineable returns true if a bucket is small enough to be written inline
// and if it contains no subbuckets. Otherwise, returns false.
func (b *Bucket) inlineable() bool {
	var n = b.rootNode

	// Bucket must only contain a single leaf node.
	if n == nil || !n.isLeaf {
		return false
	}

	// Bucket is not inlineable if it contains subbuckets or if it goes beyond
	// our threshold for inline bucket size.
	var size = common.PageHeaderSize
	for _, inode := range n.inodes {
		size += common.LeafPageElementSize + uintptr(len(inode.Key())) + uintptr(len(inode.Value()))

		if inode.Flags()&common.BucketLeafFlag != 0 {
			return false
		} else if size > b.maxInlineBucketSize() {
			return false
		}
	}

	return true
}

// Returns the maximum total size of a bucket to make it a candidate for inlining.
func (b *Bucket) maxInlineBucketSize() uintptr {
	return uintptr(b.tx.db.pageSize / 4)
}

// write allocates and writes a bucket to a byte slice.
func (b *Bucket) write() []byte {
	// Allocate the appropriate size.
	var n = b.rootNode
	var value = make([]byte, common.BucketHeaderSize+n.size())

	// Write a bucket header.
	var bucket = (*common.InBucket)(unsafe.Pointer(&value[0]))
	*bucket = *b.InBucket

	// Convert byte slice to a fake page and write the root node.
	var p = (*common.Page)(unsafe.Pointer(&value[common.BucketHeaderSize]))
	n.write(p)

	return value
}

// rebalance attempts to balance all nodes.
func (b *Bucket) rebalance() {
	for _, n := range b.nodes {
		n.rebalance()
	}
	for _, child := range b.buckets {
		child.rebalance()
	}
}

// node creates a node from a page and associates it with a given parent.
func (b *Bucket) node(pgId common.Pgid, parent *node) *node {
	common.Assert(b.nodes != nil, "nodes map expected")

	// Retrieve node if it's already been created.
	if n := b.nodes[pgId]; n != nil {
		return n
	}

	// Otherwise create a node and cache it.
	n := &node{bucket: b, parent: parent}
	if parent == nil {
		b.rootNode = n
	} else {
		parent.children = append(parent.children, n)
	}

	// Use the inline page if this is an inline bucket.
	var p = b.page
	if p == nil {
		p = b.tx.page(pgId)
	} else {
		// if p isn't nil, then it's an inline bucket.
		// The pgId must be 0 in this case.
		common.Verify(func() {
			common.Assert(pgId == 0, "The page ID (%d) isn't 0 for an inline bucket", pgId)
		})
	}

	// Read the page into the node and cache it.
	n.read(p)
	b.nodes[pgId] = n

	// Update statistics.
	b.tx.stats.IncNodeCount(1)

	return n
}

// free recursively frees all pages in the bucket.
func (b *Bucket) free() {
	if b.RootPage() == 0 {
		return
	}

	var tx = b.tx
	b.forEachPageNode(func(p *common.Page, n *node, _ int) {
		if p != nil {
			tx.db.freelist.Free(tx.meta.Txid(), p)
		} else {
			n.free()
		}
	})
	b.SetRootPage(0)
}

// dereference removes all references to the old mmap.
func (b *Bucket) dereference() {
	if b.rootNode != nil {
		b.rootNode.root().dereference()
	}

	for _, child := range b.buckets {
		child.dereference()
	}
}

// pageNode returns the in-memory node, if it exists.
// Otherwise, returns the underlying page.
func (b *Bucket) pageNode(id common.Pgid) (*common.Page, *node) {
	// Inline buckets have a fake page embedded in their value so treat them
	// differently. We'll return the rootNode (if available) or the fake page.
	if b.RootPage() == 0 {
		if id != 0 {
			panic(fmt.Sprintf("inline bucket non-zero page access(2): %d != 0", id))
		}
		if b.rootNode != nil {
			return nil, b.rootNode
		}
		return b.page, nil
	}

	// Check the node cache for non-inline buckets.
	if b.nodes != nil {
		if n := b.nodes[id]; n != nil {
			return nil, n
		}
	}

	// Finally lookup the page from the transaction if no node is materialized.
	return b.tx.page(id), nil
}

// BucketStats records statistics about resources used by a bucket.
type BucketStats struct {
	// Page count statistics.
	BranchPageN     int // number of logical branch pages
	BranchOverflowN int // number of physical branch overflow pages
	LeafPageN       int // number of logical leaf pages
	LeafOverflowN   int // number of physical leaf overflow pages

	// Tree statistics.
	KeyN  int // number of keys/value pairs
	Depth int // number of levels in B+tree

	// Page size utilization.
	BranchAlloc int // bytes allocated for physical branch pages
	BranchInuse int // bytes actually used for branch data
	LeafAlloc   int // bytes allocated for physical leaf pages
	LeafInuse   int // bytes actually used for leaf data

	// Bucket statistics
	BucketN           int // total number of buckets including the top bucket
	InlineBucketN     int // total number on inlined buckets
	InlineBucketInuse int // bytes used for inlined buckets (also accounted for in LeafInuse)
}

func (s *BucketStats) Add(other BucketStats) {
	s.BranchPageN += other.BranchPageN
	s.BranchOverflowN += other.BranchOverflowN
	s.LeafPageN += other.LeafPageN
	s.LeafOverflowN += other.LeafOverflowN
	s.KeyN += other.KeyN
	if s.Depth < other.Depth {
		s.Depth = other.Depth
	}
	s.BranchAlloc += other.BranchAlloc
	s.BranchInuse += other.BranchInuse
	s.LeafAlloc += other.LeafAlloc
	s.LeafInuse += other.LeafInuse

	s.BucketN += other.BucketN
	s.InlineBucketN += other.InlineBucketN
	s.InlineBucketInuse += other.InlineBucketInuse
}

// cloneBytes returns a copy of a given slice.
func cloneBytes(v []byte) []byte {
	var clone = make([]byte, len(v))
	copy(clone, v)
	return clone
}

type BucketStructure struct {
	Name     string            `json:"name"`              // name of the bucket
	KeyN     int               `json:"keyN"`              // number of key/value pairs
	Children []BucketStructure `json:"buckets,omitempty"` // child buckets
}
