package base

import "strings"

var (
	CustomZLength int
)

func getCustomZ() string {
	return strings.Repeat("z", CustomZLength)
}
