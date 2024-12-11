package bbolt

import (
	"encoding/hex"
	"fmt"

	"go.etcd.io/bbolt/internal/common"
)

// Check performs several consistency checks on the database for this transaction.
// An error is returned if any inconsistency is found.
//
// It can be safely run concurrently on a writable transaction. However, this
// incurs a high cost for large databases and databases with a lot of subbuckets
// because of caching. This overhead can be removed if running on a read-only
// transaction, however, it is not safe to execute other writer transactions at
// the same time.
//
// It also allows users to provide a customized `KVStringer` implementation,
// so that bolt can generate human-readable diagnostic messages.
func (tx *Tx) Check(options ...CheckOption) <-chan error {
	chkConfig := checkConfig{
		kvStringer: HexKVStringer(),
	}
	for _, op := range options {
		op(&chkConfig)
	}

	ch := make(chan error)
	go func() {
		// Close the channel to signal completion.
		defer close(ch)
		tx.check(chkConfig, ch)
	}()
	return ch
}

func (tx *Tx) check(cfg checkConfig, ch chan error) {
	// Force loading free list if opened in ReadOnly mode.
	tx.db.loadFreelist()

	// Check if any pages are double freed.
	freed := make(map[common.Pgid]bool)
	all := make([]common.Pgid, tx.db.freelist.Count())
	tx.db.freelist.Copyall(all)
	for _, id := range all {
		if freed[id] {
			ch <- fmt.Errorf("page %d: already freed", id)
		}
		freed[id] = true
	}

	// Track every reachable page.
	reachable := make(map[common.Pgid]*common.Page)
	reachable[0] = tx.page(0) // meta0
	reachable[1] = tx.page(1) // meta1
	if tx.meta.Freelist() != common.PgidNoFreelist {
		for i := uint32(0); i <= tx.page(tx.meta.Freelist()).Overflow(); i++ {
			reachable[tx.meta.Freelist()+common.Pgid(i)] = tx.page(tx.meta.Freelist())
		}
	}

	if cfg.pageId == 0 {
		// Check the whole db file, starting from the root bucket and
		// recursively check all child buckets.
		tx.recursivelyCheckBucket(&tx.root, reachable, freed, cfg.kvStringer, ch)

		// Ensure all pages below high water mark are either reachable or freed.
		for i := common.Pgid(0); i < tx.meta.Pgid(); i++ {
			_, isReachable := reachable[i]
			if !isReachable && !freed[i] {
				ch <- fmt.Errorf("page %d: unreachable unfreed", int(i))
			}
		}
	} else {
		// Check the db file starting from a specified pageId.
		if cfg.pageId < 2 || cfg.pageId >= uint64(tx.meta.Pgid()) {
			ch <- fmt.Errorf("page ID (%d) out of range [%d, %d)", cfg.pageId, 2, tx.meta.Pgid())
			return
		}

		tx.recursivelyCheckPage(common.Pgid(cfg.pageId), reachable, freed, cfg.kvStringer, ch)
	}
}

func (tx *Tx) recursivelyCheckPage(pageId common.Pgid, reachable map[common.Pgid]*common.Page, freed map[common.Pgid]bool,
	kvStringer KVStringer, ch chan error) {
	tx.checkInvariantProperties(pageId, reachable, freed, kvStringer, ch)
	tx.recursivelyCheckBucketInPage(pageId, reachable, freed, kvStringer, ch)
}

func (tx *Tx) recursivelyCheckBucketInPage(pageId common.Pgid, reachable map[common.Pgid]*common.Page, freed map[common.Pgid]bool,
	kvStringer KVStringer, ch chan error) {
	p := tx.page(pageId)

	switch {
	case p.IsBranchPage():
		for i := range p.BranchPageElements() {
			elem := p.BranchPageElement(uint16(i))
			tx.recursivelyCheckBucketInPage(elem.Pgid(), reachable, freed, kvStringer, ch)
		}
	case p.IsLeafPage():
		for i := range p.LeafPageElements() {
			elem := p.LeafPageElement(uint16(i))
			if elem.IsBucketEntry() {
				inBkt := common.NewInBucket(pageId, 0)
				tmpBucket := Bucket{
					InBucket:    &inBkt,
					rootNode:    &node{isLeaf: p.IsLeafPage()},
					FillPercent: DefaultFillPercent,
					tx:          tx,
				}
				if child := tmpBucket.Bucket(elem.Key()); child != nil {
					tx.recursivelyCheckBucket(child, reachable, freed, kvStringer, ch)
				}
			}
		}
	default:
		ch <- fmt.Errorf("unexpected page type (flags: %x) for pgId:%d", p.Flags(), pageId)
	}
}

func (tx *Tx) recursivelyCheckBucket(b *Bucket, reachable map[common.Pgid]*common.Page, freed map[common.Pgid]bool,
	kvStringer KVStringer, ch chan error) {
	// Ignore inline buckets.
	if b.RootPage() == 0 {
		return
	}

	tx.checkInvariantProperties(b.RootPage(), reachable, freed, kvStringer, ch)

	// Check each bucket within this bucket.
	_ = b.ForEachBucket(func(k []byte) error {
		if child := b.Bucket(k); child != nil {
			tx.recursivelyCheckBucket(child, reachable, freed, kvStringer, ch)
		}
		return nil
	})
}

func (tx *Tx) checkInvariantProperties(pageId common.Pgid, reachable map[common.Pgid]*common.Page, freed map[common.Pgid]bool,
	kvStringer KVStringer, ch chan error) {
	tx.forEachPage(pageId, func(p *common.Page, _ int, stack []common.Pgid) {
		verifyPageReachable(p, tx.meta.Pgid(), stack, reachable, freed, ch)
	})

	tx.recursivelyCheckPageKeyOrder(pageId, kvStringer.KeyToString, ch)
}

func verifyPageReachable(p *common.Page, hwm common.Pgid, stack []common.Pgid, reachable map[common.Pgid]*common.Page, freed map[common.Pgid]bool, ch chan error) {
	if p.Id() > hwm {
		ch <- fmt.Errorf("page %d: out of bounds: %d (stack: %v)", int(p.Id()), int(hwm), stack)
	}

	// Ensure each page is only referenced once.
	for i := common.Pgid(0); i <= common.Pgid(p.Overflow()); i++ {
		var id = p.Id() + i
		if _, ok := reachable[id]; ok {
			ch <- fmt.Errorf("page %d: multiple references (stack: %v)", int(id), stack)
		}
		reachable[id] = p
	}

	// We should only encounter un-freed leaf and branch pages.
	if freed[p.Id()] {
		ch <- fmt.Errorf("page %d: reachable freed", int(p.Id()))
	} else if !p.IsBranchPage() && !p.IsLeafPage() {
		ch <- fmt.Errorf("page %d: invalid type: %s (stack: %v)", int(p.Id()), p.Typ(), stack)
	}
}

// recursivelyCheckPageKeyOrder verifies database consistency with respect to b-tree
// key order constraints:
//   - keys on pages must be sorted
//   - keys on children pages are between 2 consecutive keys on the parent's branch page).
func (tx *Tx) recursivelyCheckPageKeyOrder(pgId common.Pgid, keyToString func([]byte) string, ch chan error) {
	tx.recursivelyCheckPageKeyOrderInternal(pgId, nil, nil, nil, keyToString, ch)
}

// recursivelyCheckPageKeyOrderInternal verifies that all keys in the subtree rooted at `pgid` are:
//   - >=`minKeyClosed` (can be nil)
//   - <`maxKeyOpen` (can be nil)
//   - Are in right ordering relationship to their parents.
//     `pagesStack` is expected to contain IDs of pages from the tree root to `pgid` for the clean debugging message.
func (tx *Tx) recursivelyCheckPageKeyOrderInternal(
	pgId common.Pgid, minKeyClosed, maxKeyOpen []byte, pagesStack []common.Pgid,
	keyToString func([]byte) string, ch chan error) (maxKeyInSubtree []byte) {

	p := tx.page(pgId)
	pagesStack = append(pagesStack, pgId)
	switch {
	case p.IsBranchPage():
		// For branch page we navigate ranges of all subpages.
		runningMin := minKeyClosed
		for i := range p.BranchPageElements() {
			elem := p.BranchPageElement(uint16(i))
			verifyKeyOrder(elem.Pgid(), "branch", i, elem.Key(), runningMin, maxKeyOpen, ch, keyToString, pagesStack)

			maxKey := maxKeyOpen
			if i < len(p.BranchPageElements())-1 {
				maxKey = p.BranchPageElement(uint16(i + 1)).Key()
			}
			maxKeyInSubtree = tx.recursivelyCheckPageKeyOrderInternal(elem.Pgid(), elem.Key(), maxKey, pagesStack, keyToString, ch)
			runningMin = maxKeyInSubtree
		}
		return maxKeyInSubtree
	case p.IsLeafPage():
		runningMin := minKeyClosed
		for i := range p.LeafPageElements() {
			elem := p.LeafPageElement(uint16(i))
			verifyKeyOrder(pgId, "leaf", i, elem.Key(), runningMin, maxKeyOpen, ch, keyToString, pagesStack)
			runningMin = elem.Key()
		}
		if p.Count() > 0 {
			return p.LeafPageElement(p.Count() - 1).Key()
		}
	default:
		ch <- fmt.Errorf("unexpected page type (flags: %x) for pgId:%d", p.Flags(), pgId)
	}
	return maxKeyInSubtree
}

/***
 * verifyKeyOrder checks whether an entry with given #index on pgId (pageType: "branch|leaf") that has given "key",
 * is within range determined by (previousKey..maxKeyOpen) and reports found violations to the channel (ch).
 */
func verifyKeyOrder(pgId common.Pgid, pageType string, index int, key []byte, previousKey []byte, maxKeyOpen []byte, ch chan error, keyToString func([]byte) string, pagesStack []common.Pgid) {
	if index == 0 && previousKey != nil && compareKeys(previousKey, key) > 0 {
		ch <- fmt.Errorf("the first key[%d]=(hex)%s on %s page(%d) needs to be >= the key in the ancestor (%s). Stack: %v",
			index, keyToString(key), pageType, pgId, keyToString(previousKey), pagesStack)
	}
	if index > 0 {
		cmpRet := compareKeys(previousKey, key)
		if cmpRet > 0 {
			ch <- fmt.Errorf("key[%d]=(hex)%s on %s page(%d) needs to be > (found <) than previous element (hex)%s. Stack: %v",
				index, keyToString(key), pageType, pgId, keyToString(previousKey), pagesStack)
		}
		if cmpRet == 0 {
			ch <- fmt.Errorf("key[%d]=(hex)%s on %s page(%d) needs to be > (found =) than previous element (hex)%s. Stack: %v",
				index, keyToString(key), pageType, pgId, keyToString(previousKey), pagesStack)
		}
	}
	if maxKeyOpen != nil && compareKeys(key, maxKeyOpen) >= 0 {
		ch <- fmt.Errorf("key[%d]=(hex)%s on %s page(%d) needs to be < than key of the next element in ancestor (hex)%s. Pages stack: %v",
			index, keyToString(key), pageType, pgId, keyToString(previousKey), pagesStack)
	}
}

// ===========================================================================================

type checkConfig struct {
	kvStringer KVStringer
	pageId     uint64
}

type CheckOption func(options *checkConfig)

func WithKVStringer(kvStringer KVStringer) CheckOption {
	return func(c *checkConfig) {
		c.kvStringer = kvStringer
	}
}

// WithPageId sets a page ID from which the check command starts to check
func WithPageId(pageId uint64) CheckOption {
	return func(c *checkConfig) {
		c.pageId = pageId
	}
}

// KVStringer allows to prepare human-readable diagnostic messages.
type KVStringer interface {
	KeyToString([]byte) string
	ValueToString([]byte) string
}

// HexKVStringer serializes both key & value to hex representation.
func HexKVStringer() KVStringer {
	return hexKvStringer{}
}

type hexKvStringer struct{}

func (_ hexKvStringer) KeyToString(key []byte) string {
	return hex.EncodeToString(key)
}

func (_ hexKvStringer) ValueToString(value []byte) string {
	return hex.EncodeToString(value)
}
