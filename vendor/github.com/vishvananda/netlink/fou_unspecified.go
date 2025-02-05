//go:build !linux
// +build !linux

package netlink

func FouAdd(f Fou) error {
	return ErrNotImplemented
}

func FouDel(f Fou) error {
	return ErrNotImplemented
}

func FouList(fam int) ([]Fou, error) {
	return nil, ErrNotImplemented
}
