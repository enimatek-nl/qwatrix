package matrix

import "time"

// timeNowUnixNano exists so the identifier generator's seed is one obvious expression.
func timeNowUnixNano() int64 { return time.Now().UnixNano() }
