//go:build arenadebug

package arena

const Debug = true
const poisonWord = 0xDEADBEEFDEADBEEF

func poison(w []uint64) {
	for i := range w {
		w[i] = poisonWord
	}
}
