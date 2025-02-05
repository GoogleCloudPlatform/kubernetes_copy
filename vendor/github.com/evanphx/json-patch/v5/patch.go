package jsonpatch

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/evanphx/json-patch/v5/internal/json"
)

const (
	eRaw = iota
	eDoc
	eAry
)

var (
	// SupportNegativeIndices decides whether to support non-standard practice of
	// allowing negative indices to mean indices starting at the end of an array.
	// Default to true.
	SupportNegativeIndices bool = true
	// AccumulatedCopySizeLimit limits the total size increase in bytes caused by
	// "copy" operations in a patch.
	AccumulatedCopySizeLimit int64 = 0
	startObject                    = json.Delim('{')
	endObject                      = json.Delim('}')
	startArray                     = json.Delim('[')
	endArray                       = json.Delim(']')
)

var (
	ErrTestFailed   = errors.New("test failed")
	ErrMissing      = errors.New("missing value")
	ErrUnknownType  = errors.New("unknown object type")
	ErrInvalid      = errors.New("invalid state detected")
	ErrInvalidIndex = errors.New("invalid index referenced")

	ErrExpectedObject = errors.New("invalid value, expected object")

	rawJSONArray  = []byte("[]")
	rawJSONObject = []byte("{}")
	rawJSONNull   = []byte("null")
)

type lazyNode struct {
	raw   *json.RawMessage
	doc   *partialDoc
	ary   *partialArray
	which int
}

// Operation is a single JSON-Patch step, such as a single 'add' operation.
type Operation map[string]*json.RawMessage

// Patch is an ordered collection of Operations.
type Patch []Operation

type partialDoc struct {
	self *lazyNode
	keys []string
	obj  map[string]*lazyNode

	opts *ApplyOptions
}

type partialArray struct {
	self  *lazyNode
	nodes []*lazyNode
}

type container interface {
	get(key string, options *ApplyOptions) (*lazyNode, error)
	set(key string, val *lazyNode, options *ApplyOptions) error
	add(key string, val *lazyNode, options *ApplyOptions) error
	remove(key string, options *ApplyOptions) error
}

// ApplyOptions specifies options for calls to ApplyWithOptions.
// Use NewApplyOptions to obtain default values for ApplyOptions.
type ApplyOptions struct {
	// SupportNegativeIndices decides whether to support non-standard practice of
	// allowing negative indices to mean indices starting at the end of an array.
	// Default to true.
	SupportNegativeIndices bool
	// AccumulatedCopySizeLimit limits the total size increase in bytes caused by
	// "copy" operations in a patch.
	AccumulatedCopySizeLimit int64
	// AllowMissingPathOnRemove indicates whether to fail "remove" operations when the target path is missing.
	// Default to false.
	AllowMissingPathOnRemove bool
	// EnsurePathExistsOnAdd instructs json-patch to recursively create the missing parts of path on "add" operation.
	// Default to false.
	EnsurePathExistsOnAdd bool

	EscapeHTML bool
}

// NewApplyOptions creates a default set of options for calls to ApplyWithOptions.
func NewApplyOptions() *ApplyOptions {
	return &ApplyOptions{
		SupportNegativeIndices:   SupportNegativeIndices,
		AccumulatedCopySizeLimit: AccumulatedCopySizeLimit,
		AllowMissingPathOnRemove: false,
		EnsurePathExistsOnAdd:    false,
		EscapeHTML:               true,
	}
}

func newLazyNode(raw *json.RawMessage) *lazyNode {
	return &lazyNode{raw: raw, doc: nil, ary: nil, which: eRaw}
}

func newRawMessage(buf []byte) *json.RawMessage {
	ra := make(json.RawMessage, len(buf))
	copy(ra, buf)
	return &ra
}

func (n *lazyNode) RedirectMarshalJSON() (any, error) {
	switch n.which {
	case eRaw:
		return n.raw, nil
	case eDoc:
		return n.doc, nil
	case eAry:
		return n.ary.nodes, nil
	default:
		return nil, ErrUnknownType
	}
}

func (n *lazyNode) UnmarshalJSON(data []byte) error {
	dest := make(json.RawMessage, len(data))
	copy(dest, data)
	n.raw = &dest
	n.which = eRaw
	return nil
}

func (n *partialDoc) TrustMarshalJSON(buf *bytes.Buffer) error {
	if n.obj == nil {
		return ErrExpectedObject
	}

	if err := buf.WriteByte('{'); err != nil {
		return err
	}
	escaped := true

	// n.opts should always be set, but in case we missed a case,
	// guard.
	if n.opts != nil {
		escaped = n.opts.EscapeHTML
	}

	for i, k := range n.keys {
		if i > 0 {
			if err := buf.WriteByte(','); err != nil {
				return err
			}
		}
		key, err := json.MarshalEscaped(k, escaped)
		if err != nil {
			return err
		}
		if _, err := buf.Write(key); err != nil {
			return err
		}
		if err := buf.WriteByte(':'); err != nil {
			return err
		}
		value, err := json.MarshalEscaped(n.obj[k], escaped)
		if err != nil {
			return err
		}
		if _, err := buf.Write(value); err != nil {
			return err
		}
	}
	if err := buf.WriteByte('}'); err != nil {
		return err
	}
	return nil
}

type syntaxError struct {
	msg string
}

func (err *syntaxError) Error() string {
	return err.msg
}

func (n *partialDoc) UnmarshalJSON(data []byte) error {
	keys, err := json.UnmarshalValidWithKeys(data, &n.obj)
	if err != nil {
		return err
	}

	n.keys = keys

	return nil
}

func (n *partialArray) UnmarshalJSON(data []byte) error {
	return json.UnmarshalValid(data, &n.nodes)
}

func (n *partialArray) RedirectMarshalJSON() (interface{}, error) {
	return n.nodes, nil
}

func deepCopy(src *lazyNode, options *ApplyOptions) (*lazyNode, int, error) {
	if src == nil {
		return nil, 0, nil
	}
	a, err := json.MarshalEscaped(src, options.EscapeHTML)
	if err != nil {
		return nil, 0, err
	}
	sz := len(a)
	return newLazyNode(newRawMessage(a)), sz, nil
}

func (n *lazyNode) nextByte() byte {
	s := []byte(*n.raw)

	for unicode.IsSpace(rune(s[0])) {
		s = s[1:]
	}

	return s[0]
}

func (n *lazyNode) intoDoc(options *ApplyOptions) (*partialDoc, error) {
	if n.which == eDoc {
		return n.doc, nil
	}

	if n.raw == nil {
		return nil, ErrInvalid
	}

	if n.nextByte() != '{' {
		return nil, ErrInvalid
	}

	err := unmarshal(*n.raw, &n.doc)

	if n.doc == nil {
		return nil, ErrInvalid
	}

	n.doc.opts = options
	if err != nil {
		return nil, err
	}

	n.which = eDoc
	return n.doc, nil
}

func (n *lazyNode) intoAry() (*partialArray, error) {
	if n.which == eAry {
		return n.ary, nil
	}

	if n.raw == nil {
		return nil, ErrInvalid
	}

	err := unmarshal(*n.raw, &n.ary)

	if err != nil {
		return nil, err
	}

	n.which = eAry
	return n.ary, nil
}

func (n *lazyNode) compact() []byte {
	buf := &bytes.Buffer{}

	if n.raw == nil {
		return nil
	}

	err := json.Compact(buf, *n.raw)

	if err != nil {
		return *n.raw
	}

	return buf.Bytes()
}

func (n *lazyNode) tryDoc() bool {
	if n.raw == nil {
		return false
	}

	err := unmarshal(*n.raw, &n.doc)

	if err != nil {
		return false
	}

	if n.doc == nil {
		return false
	}

	n.which = eDoc
	return true
}

func (n *lazyNode) tryAry() bool {
	if n.raw == nil {
		return false
	}

	err := unmarshal(*n.raw, &n.ary)

	if err != nil {
		return false
	}

	n.which = eAry
	return true
}

func (n *lazyNode) isNull() bool {
	if n == nil {
		return true
	}

	if n.raw == nil {
		return true
	}

	return bytes.Equal(n.compact(), rawJSONNull)
}

func (n *lazyNode) equal(o *lazyNode) bool {
	if n.which == eRaw {
		if !n.tryDoc() && !n.tryAry() {
			if o.which != eRaw {
				return false
			}

			nc := n.compact()
			oc := o.compact()

			if nc[0] == '"' && oc[0] == '"' {
				// ok, 2 strings

				var ns, os string

				err := json.UnmarshalValid(nc, &ns)
				if err != nil {
					return false
				}
				err = json.UnmarshalValid(oc, &os)
				if err != nil {
					return false
				}

				return ns == os
			}

			return bytes.Equal(nc, oc)
		}
	}

	if n.which == eDoc {
		if o.which == eRaw {
			if !o.tryDoc() {
				return false
			}
		}

		if o.which != eDoc {
			return false
		}

		if len(n.doc.obj) != len(o.doc.obj) {
			return false
		}

		for k, v := range n.doc.obj {
			ov, ok := o.doc.obj[k]

			if !ok {
				return false
			}

			if (v == nil) != (ov == nil) {
				return false
			}

			if v == nil && ov == nil {
				continue
			}

			if !v.equal(ov) {
				return false
			}
		}

		return true
	}

	if o.which != eAry && !o.tryAry() {
		return false
	}

	if len(n.ary.nodes) != len(o.ary.nodes) {
		return false
	}

	for idx, val := range n.ary.nodes {
		if !val.equal(o.ary.nodes[idx]) {
			return false
		}
	}

	return true
}

// Kind reads the "op" field of the Operation.
func (o Operation) Kind() string {
	if obj, ok := o["op"]; ok && obj != nil {
		var op string

		err := unmarshal(*obj, &op)

		if err != nil {
			return "unknown"
		}

		return op
	}

	return "unknown"
}

// Path reads the "path" field of the Operation.
func (o Operation) Path() (string, error) {
	if obj, ok := o["path"]; ok && obj != nil {
		var op string

		err := unmarshal(*obj, &op)

		if err != nil {
			return "unknown", err
		}

		return op, nil
	}

	return "unknown", fmt.Errorf("operation missing path field: %w", ErrMissing)
}

// From reads the "from" field of the Operation.
func (o Operation) From() (string, error) {
	if obj, ok := o["from"]; ok && obj != nil {
		var op string

		err := unmarshal(*obj, &op)

		if err != nil {
			return "unknown", err
		}

		return op, nil
	}

	return "unknown", fmt.Errorf("operation, missing from field: %w", ErrMissing)
}

func (o Operation) value() *lazyNode {
	if obj, ok := o["value"]; ok {
		// A `null` gets decoded as a nil RawMessage, so let's fix it up here.
		if obj == nil {
			return newLazyNode(newRawMessage(rawJSONNull))
		}
		return newLazyNode(obj)
	}

	return nil
}

// ValueInterface decodes the operation value into an interface.
func (o Operation) ValueInterface() (interface{}, error) {
	if obj, ok := o["value"]; ok {
		if obj == nil {
			return nil, nil
		}

		var v interface{}

		err := unmarshal(*obj, &v)

		if err != nil {
			return nil, err
		}

		return v, nil
	}

	return nil, fmt.Errorf("operation, missing value field: %w", ErrMissing)
}

func isArray(buf []byte) bool {
Loop:
	for _, c := range buf {
		switch c {
		case ' ':
		case '\n':
		case '\t':
			continue
		case '[':
			return true
		default:
			break Loop
		}
	}

	return false
}

func findObject(pd *container, path string, options *ApplyOptions) (container, string) {
	doc := *pd

	split := strings.Split(path, "/")

	if len(split) < 2 {
		if path == "" {
			return doc, ""
		}
		return nil, ""
	}

	parts := split[1 : len(split)-1]

	key := split[len(split)-1]

	var err error

	for _, part := range parts {

		next, ok := doc.get(decodePatchKey(part), options)

		if next == nil || ok != nil {
			return nil, ""
		}

		if isArray(*next.raw) {
			doc, err = next.intoAry()

			if err != nil {
				return nil, ""
			}
		} else {
			doc, err = next.intoDoc(options)

			if err != nil {
				return nil, ""
			}
		}
	}

	return doc, decodePatchKey(key)
}

func (d *partialDoc) set(key string, val *lazyNode, options *ApplyOptions) error {
	if d.obj == nil {
		return ErrExpectedObject
	}

	found := false
	for _, k := range d.keys {
		if k == key {
			found = true
			break
		}
	}
	if !found {
		d.keys = append(d.keys, key)
	}
	d.obj[key] = val
	return nil
}

func (d *partialDoc) add(key string, val *lazyNode, options *ApplyOptions) error {
	return d.set(key, val, options)
}

func (d *partialDoc) get(key string, options *ApplyOptions) (*lazyNode, error) {
	if key == "" {
		return d.self, nil
	}

	if d.obj == nil {
		return nil, ErrExpectedObject
	}

	v, ok := d.obj[key]
	if !ok {
		return v, fmt.Errorf("unable to get nonexistent key: %s: %w", key, ErrMissing)
	}
	return v, nil
}

func (d *partialDoc) remove(key string, options *ApplyOptions) error {
	if d.obj == nil {
		return ErrExpectedObject
	}

	_, ok := d.obj[key]
	if !ok {
		if options.AllowMissingPathOnRemove {
			return nil
		}
		return fmt.Errorf("unable to remove nonexistent key: %s: %w", key, ErrMissing)
	}
	idx := -1
	for i, k := range d.keys {
		if k == key {
			idx = i
			break
		}
	}
	d.keys = append(d.keys[0:idx], d.keys[idx+1:]...)
	delete(d.obj, key)
	return nil
}

// set should only be used to implement the "replace" operation, so "key" must
// be an already existing index in "d".
func (d *partialArray) set(key string, val *lazyNode, options *ApplyOptions) error {
	idx, err := strconv.Atoi(key)
	if err != nil {
		return err
	}

	if idx < 0 {
		if !options.SupportNegativeIndices {
			return fmt.Errorf("Unable to access invalid index: %d: %w", idx, ErrInvalidIndex)
		}
		if idx < -len(d.nodes) {
			return fmt.Errorf("Unable to access invalid index: %d: %w", idx, ErrInvalidIndex)
		}
		idx += len(d.nodes)
	}

	d.nodes[idx] = val
	return nil
}

func (d *partialArray) add(key string, val *lazyNode, options *ApplyOptions) error {
	if key == "-" {
		d.nodes = append(d.nodes, val)
		return nil
	}

	idx, err := strconv.Atoi(key)
	if err != nil {
		return fmt.Errorf("value was not a proper array index: '%s': %w", key, err)
	}

	sz := len(d.nodes) + 1

	ary := make([]*lazyNode, sz)

	cur := d

	if idx >= len(ary) {
		return fmt.Errorf("Unable to access invalid index: %d: %w", idx, ErrInvalidIndex)
	}

	if idx < 0 {
		if !options.SupportNegativeIndices {
			return fmt.Errorf("Unable to access invalid index: %d: %w", idx, ErrInvalidIndex)
		}
		if idx < -len(ary) {
			return fmt.Errorf("Unable to access invalid index: %d: %w", idx, ErrInvalidIndex)
		}
		idx += len(ary)
	}

	copy(ary[0:idx], cur.nodes[0:idx])
	ary[idx] = val
	copy(ary[idx+1:], cur.nodes[idx:])

	d.nodes = ary
	return nil
}

func (d *partialArray) get(key string, options *ApplyOptions) (*lazyNode, error) {
	if key == "" {
		return d.self, nil
	}

	idx, err := strconv.Atoi(key)

	if err != nil {
		return nil, err
	}

	if idx < 0 {
		if !options.SupportNegativeIndices {
			return nil, fmt.Errorf("Unable to access invalid index: %d: %w", idx, ErrInvalidIndex)
		}
		if idx < -len(d.nodes) {
			return nil, fmt.Errorf("Unable to access invalid index: %d: %w", idx, ErrInvalidIndex)
		}
		idx += len(d.nodes)
	}

	if idx >= len(d.nodes) {
		return nil, fmt.Errorf("Unable to access invalid index: %d: %w", idx, ErrInvalidIndex)
	}

	return d.nodes[idx], nil
}

func (d *partialArray) remove(key string, options *ApplyOptions) error {
	idx, err := strconv.Atoi(key)
	if err != nil {
		return err
	}

	cur := d

	if idx >= len(cur.nodes) {
		if options.AllowMissingPathOnRemove {
			return nil
		}
		return fmt.Errorf("Unable to access invalid index: %d: %w", idx, ErrInvalidIndex)
	}

	if idx < 0 {
		if !options.SupportNegativeIndices {
			return fmt.Errorf("Unable to access invalid index: %d: %w", idx, ErrInvalidIndex)
		}
		if idx < -len(cur.nodes) {
			if options.AllowMissingPathOnRemove {
				return nil
			}
			return fmt.Errorf("Unable to access invalid index: %d: %w", idx, ErrInvalidIndex)
		}
		idx += len(cur.nodes)
	}

	ary := make([]*lazyNode, len(cur.nodes)-1)

	copy(ary[0:idx], cur.nodes[0:idx])
	copy(ary[idx:], cur.nodes[idx+1:])

	d.nodes = ary
	return nil
}

func (p Patch) add(doc *container, op Operation, options *ApplyOptions) error {
	path, err := op.Path()
	if err != nil {
		return fmt.Errorf("add operation failed to decode path: %w", ErrMissing)
	}

	// special case, adding to empty means replacing the container with the value given
	if path == "" {
		val := op.value()

		var pd container
		if (*val.raw)[0] == '[' {
			pd = &partialArray{
				self: val,
			}
		} else {
			pd = &partialDoc{
				self: val,
				opts: options,
			}
		}

		err := json.UnmarshalValid(*val.raw, pd)

		if err != nil {
			return err
		}

		*doc = pd

		return nil
	}

	if options.EnsurePathExistsOnAdd {
		err = ensurePathExists(doc, path, options)

		if err != nil {
			return err
		}
	}

	con, key := findObject(doc, path, options)

	if con == nil {
		return fmt.Errorf("add operation does not apply: doc is missing path: \"%s\": %w", path, ErrMissing)
	}

	err = con.add(key, op.value(), options)
	if err != nil {
		return fmt.Errorf("error in add for path: '%s': %w", path, err)
	}

	return nil
}

// Given a document and a path to a key, walk the path and create all missing elements
// creating objects and arrays as needed.
func ensurePathExists(pd *container, path string, options *ApplyOptions) error {
	doc := *pd

	var err error
	var arrIndex int

	split := strings.Split(path, "/")

	if len(split) < 2 {
		return nil
	}

	parts := split[1:]

	for pi, part := range parts {

		// Have we reached the key part of the path?
		// If yes, we're done.
		if pi == len(parts)-1 {
			return nil
		}

		target, ok := doc.get(decodePatchKey(part), options)

		if target == nil || ok != nil {

			// If the current container is an array which has fewer elements than our target index,
			// pad the current container with nulls.
			if arrIndex, err = strconv.Atoi(part); err == nil {
				pa, ok := doc.(*partialArray)

				if ok && arrIndex >= len(pa.nodes)+1 {
					// Pad the array with null values up to the required index.
					for i := len(pa.nodes); i <= arrIndex-1; i++ {
						doc.add(strconv.Itoa(i), newLazyNode(newRawMessage(rawJSONNull)), options)
					}
				}
			}

			// Check if the next part is a numeric index or "-".
			// If yes, then create an array, otherwise, create an object.
			if arrIndex, err = strconv.Atoi(parts[pi+1]); err == nil || parts[pi+1] == "-" {
				if arrIndex < 0 {

					if !options.SupportNegativeIndices {
						return fmt.Errorf("Unable to ensure path for invalid index: %d: %w", arrIndex, ErrInvalidIndex)
					}

					if arrIndex < -1 {
						return fmt.Errorf("Unable to ensure path for negative index other than -1: %d: %w", arrIndex, ErrInvalidIndex)
					}

					arrIndex = 0
				}

				newNode := newLazyNode(newRawMessage(rawJSONArray))
				doc.add(part, newNode, options)
				doc, _ = newNode.intoAry()

				// Pad the new array with null values up to the required index.
				for i := 0; i < arrIndex; i++ {
					doc.add(strconv.Itoa(i), newLazyNode(newRawMessage(rawJSONNull)), options)
				}
			} else {
				newNode := newLazyNode(newRawMessage(rawJSONObject))

				doc.add(part, newNode, options)
				doc, err = newNode.intoDoc(options)
				if err != nil {
					return err
				}
			}
		} else {
			if isArray(*target.raw) {
				doc, err = target.intoAry()

				if err != nil {
					return err
				}
			} else {
				doc, err = target.intoDoc(options)

				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func validateOperation(op Operation) error {
	switch op.Kind() {
	case "add", "replace":
		if _, err := op.ValueInterface(); err != nil {
			return fmt.Errorf("failed to decode 'value': %w", err)
		}
	case "move", "copy":
		if _, err := op.From(); err != nil {
			return fmt.Errorf("failed to decode 'from': %w", err)
		}
	case "remove", "test":
	default:
		return fmt.Errorf("unsupported operation")
	}

	if _, err := op.Path(); err != nil {
		return fmt.Errorf("failed to decode 'path': %w", err)
	}

	return nil
}

func validatePatch(p Patch) error {
	for _, op := range p {
		if err := validateOperation(op); err != nil {
			opData, infoErr := json.Marshal(op)
			if infoErr != nil {
				return fmt.Errorf("invalid operation: %w", err)
			}

			return fmt.Errorf("invalid operation %s: %w", opData, err)
		}
	}

	return nil
}

func (p Patch) remove(doc *container, op Operation, options *ApplyOptions) error {
	path, err := op.Path()
	if err != nil {
		return fmt.Errorf("remove operation failed to decode path: %w", ErrMissing)
	}

	con, key := findObject(doc, path, options)

	if con == nil {
		if options.AllowMissingPathOnRemove {
			return nil
		}
		return fmt.Errorf("remove operation does not apply: doc is missing path: \"%s\": %w", path, ErrMissing)
	}

	err = con.remove(key, options)
	if err != nil {
		return fmt.Errorf("error in remove for path: '%s': %w", path, err)
	}

	return nil
}

func (p Patch) replace(doc *container, op Operation, options *ApplyOptions) error {
	path, err := op.Path()
	if err != nil {
		return fmt.Errorf("replace operation failed to decode path: %w", err)
	}

	if path == "" {
		val := op.value()

		if val.which == eRaw {
			if !val.tryDoc() {
				if !val.tryAry() {
					return fmt.Errorf("replace operation value must be object or array: %w", err)
				}
			} else {
				val.doc.opts = options
			}
		}

		switch val.which {
		case eAry:
			*doc = val.ary
		case eDoc:
			*doc = val.doc
		case eRaw:
			return fmt.Errorf("replace operation hit impossible case: %w", err)
		}

		return nil
	}

	con, key := findObject(doc, path, options)

	if con == nil {
		return fmt.Errorf("replace operation does not apply: doc is missing path: %s: %w", path, ErrMissing)
	}

	_, ok := con.get(key, options)
	if ok != nil {
		return fmt.Errorf("replace operation does not apply: doc is missing key: %s: %w", path, ErrMissing)
	}

	err = con.set(key, op.value(), options)
	if err != nil {
		return fmt.Errorf("error in remove for path: '%s': %w", path, err)
	}

	return nil
}

func (p Patch) move(doc *container, op Operation, options *ApplyOptions) error {
	from, err := op.From()
	if err != nil {
		return fmt.Errorf("move operation failed to decode from: %w", err)
	}

	if from == "" {
		return fmt.Errorf("unable to move entire document to another path: %w", ErrInvalid)
	}

	con, key := findObject(doc, from, options)

	if con == nil {
		return fmt.Errorf("move operation does not apply: doc is missing from path: %s: %w", from, ErrMissing)
	}

	val, err := con.get(key, options)
	if err != nil {
		return fmt.Errorf("error in move for path: '%s': %w", key, err)
	}

	err = con.remove(key, options)
	if err != nil {
		return fmt.Errorf("error in move for path: '%s': %w", key, err)
	}

	path, err := op.Path()
	if err != nil {
		return fmt.Errorf("move operation failed to decode path: %w", err)
	}

	con, key = findObject(doc, path, options)

	if con == nil {
		return fmt.Errorf("move operation does not apply: doc is missing destination path: %s: %w", path, ErrMissing)
	}

	err = con.add(key, val, options)
	if err != nil {
		return fmt.Errorf("error in move for path: '%s': %w", path, err)
	}

	return nil
}

func (p Patch) test(doc *container, op Operation, options *ApplyOptions) error {
	path, err := op.Path()
	if err != nil {
		return fmt.Errorf("test operation failed to decode path: %w", err)
	}

	if path == "" {
		var self lazyNode

		switch sv := (*doc).(type) {
		case *partialDoc:
			self.doc = sv
			self.which = eDoc
		case *partialArray:
			self.ary = sv
			self.which = eAry
		}

		if self.equal(op.value()) {
			return nil
		}

		return fmt.Errorf("testing value %s failed: %w", path, ErrTestFailed)
	}

	con, key := findObject(doc, path, options)

	if con == nil {
		return fmt.Errorf("test operation does not apply: is missing path: %s: %w", path, ErrMissing)
	}

	val, err := con.get(key, options)
	if err != nil && errors.Unwrap(err) != ErrMissing {
		return fmt.Errorf("error in test for path: '%s': %w", path, err)
	}

	ov := op.value()

	if val == nil {
		if ov.isNull() {
			return nil
		}
		return fmt.Errorf("testing value %s failed: %w", path, ErrTestFailed)
	} else if ov.isNull() {
		return fmt.Errorf("testing value %s failed: %w", path, ErrTestFailed)
	}

	if val.equal(op.value()) {
		return nil
	}

	return fmt.Errorf("testing value %s failed: %w", path, ErrTestFailed)
}

func (p Patch) copy(doc *container, op Operation, accumulatedCopySize *int64, options *ApplyOptions) error {
	from, err := op.From()
	if err != nil {
		return fmt.Errorf("copy operation failed to decode from: %w", err)
	}

	con, key := findObject(doc, from, options)

	if con == nil {
		return fmt.Errorf("copy operation does not apply: doc is missing from path: \"%s\": %w", from, ErrMissing)
	}

	val, err := con.get(key, options)
	if err != nil {
		return fmt.Errorf("error in copy for from: '%s': %w", from, err)
	}

	path, err := op.Path()
	if err != nil {
		return fmt.Errorf("copy operation failed to decode path: %w", ErrMissing)
	}

	con, key = findObject(doc, path, options)

	if con == nil {
		return fmt.Errorf("copy operation does not apply: doc is missing destination path: %s: %w", path, ErrMissing)
	}

	valCopy, sz, err := deepCopy(val, options)
	if err != nil {
		return fmt.Errorf("error while performing deep copy: %w", err)
	}

	(*accumulatedCopySize) += int64(sz)
	if options.AccumulatedCopySizeLimit > 0 && *accumulatedCopySize > options.AccumulatedCopySizeLimit {
		return NewAccumulatedCopySizeError(options.AccumulatedCopySizeLimit, *accumulatedCopySize)
	}

	err = con.add(key, valCopy, options)
	if err != nil {
		return fmt.Errorf("error while adding value during copy: %w", err)
	}

	return nil
}

// Equal indicates if 2 JSON documents have the same structural equality.
func Equal(a, b []byte) bool {
	la := newLazyNode(newRawMessage(a))
	lb := newLazyNode(newRawMessage(b))

	return la.equal(lb)
}

// DecodePatch decodes the passed JSON document as an RFC 6902 patch.
func DecodePatch(buf []byte) (Patch, error) {
	if !json.Valid(buf) {
		return nil, ErrInvalid
	}

	var p Patch

	err := unmarshal(buf, &p)

	if err != nil {
		return nil, err
	}

	if err := validatePatch(p); err != nil {
		return nil, err
	}

	return p, nil
}

// Apply mutates a JSON document according to the patch, and returns the new
// document.
func (p Patch) Apply(doc []byte) ([]byte, error) {
	return p.ApplyWithOptions(doc, NewApplyOptions())
}

// ApplyWithOptions mutates a JSON document according to the patch and the passed in ApplyOptions.
// It returns the new document.
func (p Patch) ApplyWithOptions(doc []byte, options *ApplyOptions) ([]byte, error) {
	return p.ApplyIndentWithOptions(doc, "", options)
}

// ApplyIndent mutates a JSON document according to the patch, and returns the new
// document indented.
func (p Patch) ApplyIndent(doc []byte, indent string) ([]byte, error) {
	return p.ApplyIndentWithOptions(doc, indent, NewApplyOptions())
}

// ApplyIndentWithOptions mutates a JSON document according to the patch and the passed in ApplyOptions.
// It returns the new document indented.
func (p Patch) ApplyIndentWithOptions(doc []byte, indent string, options *ApplyOptions) ([]byte, error) {
	if len(doc) == 0 {
		return doc, nil
	}

	if !json.Valid(doc) {
		return nil, ErrInvalid
	}

	raw := json.RawMessage(doc)
	self := newLazyNode(&raw)

	var pd container
	if doc[0] == '[' {
		pd = &partialArray{
			self: self,
		}
	} else {
		pd = &partialDoc{
			self: self,
			opts: options,
		}
	}

	err := unmarshal(doc, pd)

	if err != nil {
		return nil, err
	}

	err = nil

	var accumulatedCopySize int64

	for _, op := range p {
		switch op.Kind() {
		case "add":
			err = p.add(&pd, op, options)
		case "remove":
			err = p.remove(&pd, op, options)
		case "replace":
			err = p.replace(&pd, op, options)
		case "move":
			err = p.move(&pd, op, options)
		case "test":
			err = p.test(&pd, op, options)
		case "copy":
			err = p.copy(&pd, op, &accumulatedCopySize, options)
		default:
			err = fmt.Errorf("Unexpected kind: %s", op.Kind())
		}

		if err != nil {
			return nil, err
		}
	}

	data, err := json.MarshalEscaped(pd, options.EscapeHTML)
	if err != nil {
		return nil, err
	}

	if indent == "" {
		return data, nil
	}

	var buf bytes.Buffer
	json.Indent(&buf, data, "", indent)
	return buf.Bytes(), nil
}

// From http://tools.ietf.org/html/rfc6901#section-4 :
//
// Evaluation of each reference token begins by decoding any escaped
// character sequence.  This is performed by first transforming any
// occurrence of the sequence '~1' to '/', and then transforming any
// occurrence of the sequence '~0' to '~'.

var (
	rfc6901Decoder = strings.NewReplacer("~1", "/", "~0", "~")
)

func decodePatchKey(k string) string {
	return rfc6901Decoder.Replace(k)
}
