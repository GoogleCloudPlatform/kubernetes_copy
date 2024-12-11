package bbolt

import "go.etcd.io/bbolt/errors"

// These errors can be returned when opening or calling methods on a DB.
var (
	// ErrDatabaseNotOpen is returned when a DB instance is accessed before it
	// is opened or after it is closed.
	//
	// Deprecated: Use the error variables defined in the bbolt/errors package.
	ErrDatabaseNotOpen = errors.ErrDatabaseNotOpen

	// ErrInvalid is returned when both meta pages on a database are invalid.
	// This typically occurs when a file is not a bolt database.
	//
	// Deprecated: Use the error variables defined in the bbolt/errors package.
	ErrInvalid = errors.ErrInvalid

	// ErrInvalidMapping is returned when the database file fails to get mapped.
	//
	// Deprecated: Use the error variables defined in the bbolt/errors package.
	ErrInvalidMapping = errors.ErrInvalidMapping

	// ErrVersionMismatch is returned when the data file was created with a
	// different version of Bolt.
	//
	// Deprecated: Use the error variables defined in the bbolt/errors package.
	ErrVersionMismatch = errors.ErrVersionMismatch

	// ErrChecksum is returned when a checksum mismatch occurs on either of the two meta pages.
	//
	// Deprecated: Use the error variables defined in the bbolt/errors package.
	ErrChecksum = errors.ErrChecksum

	// ErrTimeout is returned when a database cannot obtain an exclusive lock
	// on the data file after the timeout passed to Open().
	//
	// Deprecated: Use the error variables defined in the bbolt/errors package.
	ErrTimeout = errors.ErrTimeout
)

// These errors can occur when beginning or committing a Tx.
var (
	// ErrTxNotWritable is returned when performing a write operation on a
	// read-only transaction.
	//
	// Deprecated: Use the error variables defined in the bbolt/errors package.
	ErrTxNotWritable = errors.ErrTxNotWritable

	// ErrTxClosed is returned when committing or rolling back a transaction
	// that has already been committed or rolled back.
	//
	// Deprecated: Use the error variables defined in the bbolt/errors package.
	ErrTxClosed = errors.ErrTxClosed

	// ErrDatabaseReadOnly is returned when a mutating transaction is started on a
	// read-only database.
	//
	// Deprecated: Use the error variables defined in the bbolt/errors package.
	ErrDatabaseReadOnly = errors.ErrDatabaseReadOnly

	// ErrFreePagesNotLoaded is returned when a readonly transaction without
	// preloading the free pages is trying to access the free pages.
	//
	// Deprecated: Use the error variables defined in the bbolt/errors package.
	ErrFreePagesNotLoaded = errors.ErrFreePagesNotLoaded
)

// These errors can occur when putting or deleting a value or a bucket.
var (
	// ErrBucketNotFound is returned when trying to access a bucket that has
	// not been created yet.
	//
	// Deprecated: Use the error variables defined in the bbolt/errors package.
	ErrBucketNotFound = errors.ErrBucketNotFound

	// ErrBucketExists is returned when creating a bucket that already exists.
	//
	// Deprecated: Use the error variables defined in the bbolt/errors package.
	ErrBucketExists = errors.ErrBucketExists

	// ErrBucketNameRequired is returned when creating a bucket with a blank name.
	//
	// Deprecated: Use the error variables defined in the bbolt/errors package.
	ErrBucketNameRequired = errors.ErrBucketNameRequired

	// ErrKeyRequired is returned when inserting a zero-length key.
	//
	// Deprecated: Use the error variables defined in the bbolt/errors package.
	ErrKeyRequired = errors.ErrKeyRequired

	// ErrKeyTooLarge is returned when inserting a key that is larger than MaxKeySize.
	//
	// Deprecated: Use the error variables defined in the bbolt/errors package.
	ErrKeyTooLarge = errors.ErrKeyTooLarge

	// ErrValueTooLarge is returned when inserting a value that is larger than MaxValueSize.
	//
	// Deprecated: Use the error variables defined in the bbolt/errors package.
	ErrValueTooLarge = errors.ErrValueTooLarge

	// ErrIncompatibleValue is returned when trying create or delete a bucket
	// on an existing non-bucket key or when trying to create or delete a
	// non-bucket key on an existing bucket key.
	//
	// Deprecated: Use the error variables defined in the bbolt/errors package.
	ErrIncompatibleValue = errors.ErrIncompatibleValue
)
