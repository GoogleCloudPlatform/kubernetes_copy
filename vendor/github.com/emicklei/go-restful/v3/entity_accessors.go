package restful

// Copyright 2015 Ernest Micklei. All rights reserved.
// Use of this source code is governed by a license
// that can be found in the LICENSE file.

import (
	"encoding/json"
	"encoding/xml"
	"strings"
	"sync"
)

var (
	MarshalIndent = json.MarshalIndent
	NewDecoder    = json.NewDecoder
	NewEncoder    = json.NewEncoder
)

// EntityReaderWriter can read and write values using an encoding such as JSON,XML.
type EntityReaderWriter interface {
	// Read a serialized version of the value from the request.
	// The Request may have a decompressing reader. Depends on Content-Encoding.
	Read(req *Request, v interface{}) error

	// Write a serialized version of the value on the response.
	// The Response may have a compressing writer. Depends on Accept-Encoding.
	// status should be a valid Http Status code
	Write(resp *Response, status int, v interface{}) error
}

// entityAccessRegistry is a singleton
var entityAccessRegistry = &entityReaderWriters{
	protection: new(sync.RWMutex),
	accessors:  map[string]EntityReaderWriter{},
}

// entityReaderWriters associates MIME to an EntityReaderWriter
type entityReaderWriters struct {
	protection *sync.RWMutex
	accessors  map[string]EntityReaderWriter
}

func init() {
	RegisterEntityAccessor(MIME_JSON, NewEntityAccessorJSON(MIME_JSON))
	RegisterEntityAccessor(MIME_XML, NewEntityAccessorXML(MIME_XML))
}

// RegisterEntityAccessor add/overrides the ReaderWriter for encoding content with this MIME type.
func RegisterEntityAccessor(mime string, erw EntityReaderWriter) {
	entityAccessRegistry.protection.Lock()
	defer entityAccessRegistry.protection.Unlock()
	entityAccessRegistry.accessors[mime] = erw
}

// NewEntityAccessorJSON returns a new EntityReaderWriter for accessing JSON content.
// This package is already initialized with such an accessor using the MIME_JSON contentType.
func NewEntityAccessorJSON(contentType string) EntityReaderWriter {
	return entityJSONAccess{ContentType: contentType}
}

// NewEntityAccessorXML returns a new EntityReaderWriter for accessing XML content.
// This package is already initialized with such an accessor using the MIME_XML contentType.
func NewEntityAccessorXML(contentType string) EntityReaderWriter {
	return entityXMLAccess{ContentType: contentType}
}

// accessorAt returns the registered ReaderWriter for this MIME type.
func (r *entityReaderWriters) accessorAt(mime string) (EntityReaderWriter, bool) {
	r.protection.RLock()
	defer r.protection.RUnlock()
	er, ok := r.accessors[mime]
	if !ok {
		// retry with reverse lookup
		// more expensive but we are in an exceptional situation anyway
		for k, v := range r.accessors {
			if strings.Contains(mime, k) {
				return v, true
			}
		}
	}
	return er, ok
}

// entityXMLAccess is a EntityReaderWriter for XML encoding
type entityXMLAccess struct {
	// This is used for setting the Content-Type header when writing
	ContentType string
}

// Read unmarshalls the value from XML
func (e entityXMLAccess) Read(req *Request, v interface{}) error {
	return xml.NewDecoder(req.Request.Body).Decode(v)
}

// Write marshalls the value to JSON and set the Content-Type Header.
func (e entityXMLAccess) Write(resp *Response, status int, v interface{}) error {
	return writeXML(resp, status, e.ContentType, v)
}

// writeXML marshalls the value to JSON and set the Content-Type Header.
func writeXML(resp *Response, status int, contentType string, v interface{}) error {
	if v == nil {
		resp.WriteHeader(status)
		// do not write a nil representation
		return nil
	}
	if resp.prettyPrint {
		// pretty output must be created and written explicitly
		output, err := xml.MarshalIndent(v, " ", " ")
		if err != nil {
			return err
		}
		resp.Header().Set(HEADER_ContentType, contentType)
		resp.WriteHeader(status)
		_, err = resp.Write([]byte(xml.Header))
		if err != nil {
			return err
		}
		_, err = resp.Write(output)
		return err
	}
	// not-so-pretty
	resp.Header().Set(HEADER_ContentType, contentType)
	resp.WriteHeader(status)
	return xml.NewEncoder(resp).Encode(v)
}

// entityJSONAccess is a EntityReaderWriter for JSON encoding
type entityJSONAccess struct {
	// This is used for setting the Content-Type header when writing
	ContentType string
}

// Read unmarshalls the value from JSON
func (e entityJSONAccess) Read(req *Request, v interface{}) error {
	decoder := NewDecoder(req.Request.Body)
	decoder.UseNumber()
	return decoder.Decode(v)
}

// Write marshalls the value to JSON and set the Content-Type Header.
func (e entityJSONAccess) Write(resp *Response, status int, v interface{}) error {
	return writeJSON(resp, status, e.ContentType, v)
}

// write marshalls the value to JSON and set the Content-Type Header.
func writeJSON(resp *Response, status int, contentType string, v interface{}) error {
	if v == nil {
		resp.WriteHeader(status)
		// do not write a nil representation
		return nil
	}
	if resp.prettyPrint {
		// pretty output must be created and written explicitly
		output, err := MarshalIndent(v, "", " ")
		if err != nil {
			return err
		}
		resp.Header().Set(HEADER_ContentType, contentType)
		resp.WriteHeader(status)
		_, err = resp.Write(output)
		return err
	}
	// not-so-pretty
	resp.Header().Set(HEADER_ContentType, contentType)
	resp.WriteHeader(status)
	return NewEncoder(resp).Encode(v)
}
