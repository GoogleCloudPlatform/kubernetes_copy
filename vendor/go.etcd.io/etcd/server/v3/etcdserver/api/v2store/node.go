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

package v2store

import (
	"path"
	"sort"
	"time"

	"github.com/jonboulle/clockwork"

	"go.etcd.io/etcd/server/v3/etcdserver/api/v2error"
)

// explanations of Compare function result
const (
	CompareMatch = iota
	CompareIndexNotMatch
	CompareValueNotMatch
	CompareNotMatch
)

var Permanent time.Time

// node is the basic element in the store system.
// A key-value pair will have a string value
// A directory will have a children map
type node struct {
	Path string

	CreatedIndex  uint64
	ModifiedIndex uint64

	Parent *node `json:"-"` // should not encode this field! avoid circular dependency.

	ExpireTime time.Time
	Value      string           // for key-value pair
	Children   map[string]*node // for directory

	// A reference to the store this node is attached to.
	store *store
}

// newKV creates a Key-Value pair
func newKV(store *store, nodePath string, value string, createdIndex uint64, parent *node, expireTime time.Time) *node {
	return &node{
		Path:          nodePath,
		CreatedIndex:  createdIndex,
		ModifiedIndex: createdIndex,
		Parent:        parent,
		store:         store,
		ExpireTime:    expireTime,
		Value:         value,
	}
}

// newDir creates a directory
func newDir(store *store, nodePath string, createdIndex uint64, parent *node, expireTime time.Time) *node {
	return &node{
		Path:          nodePath,
		CreatedIndex:  createdIndex,
		ModifiedIndex: createdIndex,
		Parent:        parent,
		ExpireTime:    expireTime,
		Children:      make(map[string]*node),
		store:         store,
	}
}

// IsHidden function checks if the node is a hidden node. A hidden node
// will begin with '_'
// A hidden node will not be shown via get command under a directory
// For example if we have /foo/_hidden and /foo/notHidden, get "/foo"
// will only return /foo/notHidden
func (n *node) IsHidden() bool {
	_, name := path.Split(n.Path)

	return name[0] == '_'
}

// IsPermanent function checks if the node is a permanent one.
func (n *node) IsPermanent() bool {
	// we use a uninitialized time.Time to indicate the node is a
	// permanent one.
	// the uninitialized time.Time should equal zero.
	return n.ExpireTime.IsZero()
}

// IsDir function checks whether the node is a directory.
// If the node is a directory, the function will return true.
// Otherwise the function will return false.
func (n *node) IsDir() bool {
	return n.Children != nil
}

// Read function gets the value of the node.
// If the receiver node is not a key-value pair, a "Not A File" error will be returned.
func (n *node) Read() (string, *v2error.Error) {
	if n.IsDir() {
		return "", v2error.NewError(v2error.EcodeNotFile, "", n.store.CurrentIndex)
	}

	return n.Value, nil
}

// Write function set the value of the node to the given value.
// If the receiver node is a directory, a "Not A File" error will be returned.
func (n *node) Write(value string, index uint64) *v2error.Error {
	if n.IsDir() {
		return v2error.NewError(v2error.EcodeNotFile, "", n.store.CurrentIndex)
	}

	n.Value = value
	n.ModifiedIndex = index

	return nil
}

func (n *node) expirationAndTTL(clock clockwork.Clock) (*time.Time, int64) {
	if !n.IsPermanent() {
		/* compute ttl as:
		   ceiling( (expireTime - timeNow) / nanosecondsPerSecond )
		   which ranges from 1..n
		   rather than as:
		   ( (expireTime - timeNow) / nanosecondsPerSecond ) + 1
		   which ranges 1..n+1
		*/
		ttlN := n.ExpireTime.Sub(clock.Now())
		ttl := ttlN / time.Second
		if (ttlN % time.Second) > 0 {
			ttl++
		}
		t := n.ExpireTime.UTC()
		return &t, int64(ttl)
	}
	return nil, 0
}

// List function return a slice of nodes under the receiver node.
// If the receiver node is not a directory, a "Not A Directory" error will be returned.
func (n *node) List() ([]*node, *v2error.Error) {
	if !n.IsDir() {
		return nil, v2error.NewError(v2error.EcodeNotDir, "", n.store.CurrentIndex)
	}

	nodes := make([]*node, len(n.Children))

	i := 0
	for _, node := range n.Children {
		nodes[i] = node
		i++
	}

	return nodes, nil
}

// GetChild function returns the child node under the directory node.
// On success, it returns the file node
func (n *node) GetChild(name string) (*node, *v2error.Error) {
	if !n.IsDir() {
		return nil, v2error.NewError(v2error.EcodeNotDir, n.Path, n.store.CurrentIndex)
	}

	child, ok := n.Children[name]

	if ok {
		return child, nil
	}

	return nil, nil
}

// Add function adds a node to the receiver node.
// If the receiver is not a directory, a "Not A Directory" error will be returned.
// If there is an existing node with the same name under the directory, a "Already Exist"
// error will be returned
func (n *node) Add(child *node) *v2error.Error {
	if !n.IsDir() {
		return v2error.NewError(v2error.EcodeNotDir, "", n.store.CurrentIndex)
	}

	_, name := path.Split(child.Path)

	if _, ok := n.Children[name]; ok {
		return v2error.NewError(v2error.EcodeNodeExist, "", n.store.CurrentIndex)
	}

	n.Children[name] = child

	return nil
}

// Remove function remove the node.
func (n *node) Remove(dir, recursive bool, callback func(path string)) *v2error.Error {
	if !n.IsDir() { // key-value pair
		_, name := path.Split(n.Path)

		// find its parent and remove the node from the map
		if n.Parent != nil && n.Parent.Children[name] == n {
			delete(n.Parent.Children, name)
		}

		if callback != nil {
			callback(n.Path)
		}

		if !n.IsPermanent() {
			n.store.ttlKeyHeap.remove(n)
		}

		return nil
	}

	if !dir {
		// cannot delete a directory without dir set to true
		return v2error.NewError(v2error.EcodeNotFile, n.Path, n.store.CurrentIndex)
	}

	if len(n.Children) != 0 && !recursive {
		// cannot delete a directory if it is not empty and the operation
		// is not recursive
		return v2error.NewError(v2error.EcodeDirNotEmpty, n.Path, n.store.CurrentIndex)
	}

	for _, child := range n.Children { // delete all children
		child.Remove(true, true, callback)
	}

	// delete self
	_, name := path.Split(n.Path)
	if n.Parent != nil && n.Parent.Children[name] == n {
		delete(n.Parent.Children, name)

		if callback != nil {
			callback(n.Path)
		}

		if !n.IsPermanent() {
			n.store.ttlKeyHeap.remove(n)
		}
	}

	return nil
}

func (n *node) Repr(recursive, sorted bool, clock clockwork.Clock) *NodeExtern {
	if n.IsDir() {
		node := &NodeExtern{
			Key:           n.Path,
			Dir:           true,
			ModifiedIndex: n.ModifiedIndex,
			CreatedIndex:  n.CreatedIndex,
		}
		node.Expiration, node.TTL = n.expirationAndTTL(clock)

		if !recursive {
			return node
		}

		children, _ := n.List()
		node.Nodes = make(NodeExterns, len(children))

		// we do not use the index in the children slice directly
		// we need to skip the hidden one
		i := 0

		for _, child := range children {
			if child.IsHidden() { // get will not list hidden node
				continue
			}

			node.Nodes[i] = child.Repr(recursive, sorted, clock)

			i++
		}

		// eliminate hidden nodes
		node.Nodes = node.Nodes[:i]
		if sorted {
			sort.Sort(node.Nodes)
		}

		return node
	}

	// since n.Value could be changed later, so we need to copy the value out
	value := n.Value
	node := &NodeExtern{
		Key:           n.Path,
		Value:         &value,
		ModifiedIndex: n.ModifiedIndex,
		CreatedIndex:  n.CreatedIndex,
	}
	node.Expiration, node.TTL = n.expirationAndTTL(clock)
	return node
}

func (n *node) UpdateTTL(expireTime time.Time) {
	if !n.IsPermanent() {
		if expireTime.IsZero() {
			// from ttl to permanent
			n.ExpireTime = expireTime
			// remove from ttl heap
			n.store.ttlKeyHeap.remove(n)
			return
		}

		// update ttl
		n.ExpireTime = expireTime
		// update ttl heap
		n.store.ttlKeyHeap.update(n)
		return
	}

	if expireTime.IsZero() {
		return
	}

	// from permanent to ttl
	n.ExpireTime = expireTime
	// push into ttl heap
	n.store.ttlKeyHeap.push(n)
}

// Compare function compares node index and value with provided ones.
// second result value explains result and equals to one of Compare.. constants
func (n *node) Compare(prevValue string, prevIndex uint64) (ok bool, which int) {
	indexMatch := prevIndex == 0 || n.ModifiedIndex == prevIndex
	valueMatch := prevValue == "" || n.Value == prevValue
	ok = valueMatch && indexMatch
	switch {
	case valueMatch && indexMatch:
		which = CompareMatch
	case indexMatch && !valueMatch:
		which = CompareValueNotMatch
	case valueMatch && !indexMatch:
		which = CompareIndexNotMatch
	default:
		which = CompareNotMatch
	}
	return ok, which
}

// Clone function clone the node recursively and return the new node.
// If the node is a directory, it will clone all the content under this directory.
// If the node is a key-value pair, it will clone the pair.
func (n *node) Clone() *node {
	if !n.IsDir() {
		newkv := newKV(n.store, n.Path, n.Value, n.CreatedIndex, n.Parent, n.ExpireTime)
		newkv.ModifiedIndex = n.ModifiedIndex
		return newkv
	}

	clone := newDir(n.store, n.Path, n.CreatedIndex, n.Parent, n.ExpireTime)
	clone.ModifiedIndex = n.ModifiedIndex

	for key, child := range n.Children {
		clone.Children[key] = child.Clone()
	}

	return clone
}

// recoverAndclean function help to do recovery.
// Two things need to be done: 1. recovery structure; 2. delete expired nodes
//
// If the node is a directory, it will help recover children's parent pointer and recursively
// call this function on its children.
// We check the expire last since we need to recover the whole structure first and add all the
// notifications into the event history.
func (n *node) recoverAndclean() {
	if n.IsDir() {
		for _, child := range n.Children {
			child.Parent = n
			child.store = n.store
			child.recoverAndclean()
		}
	}

	if !n.ExpireTime.IsZero() {
		n.store.ttlKeyHeap.push(n)
	}
}
