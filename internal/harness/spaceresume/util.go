package spaceresume

import (
	"encoding/json"
	"fmt"
	"strconv"
)

func itoa(n int) string { return strconv.Itoa(n) }

func splitSScan(s string, v *int) (int, error) { return fmt.Sscanf(s, "%d", v) }

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
