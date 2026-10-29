//go:build !arenadebug

package arena

const Debug = false

func poison([]uint64) {}
