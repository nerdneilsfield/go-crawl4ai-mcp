package c4a

import "strconv"

func fmtFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func fmtInt(i int) string {
	return strconv.Itoa(i)
}
