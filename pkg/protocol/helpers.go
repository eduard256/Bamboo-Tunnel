package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"
)

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func timestamp() string {
	return strconv.FormatInt(time.Now().UnixMilli(), 10)
}
