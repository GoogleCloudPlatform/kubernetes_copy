//go:build amd64 && !appengine && !noasm && gc
// +build amd64,!appengine,!noasm,gc

package cpuinfo

// go:noescape
func x86extensions() (bmi1, bmi2 bool)

func init() {
	hasBMI1, hasBMI2 = x86extensions()
}
