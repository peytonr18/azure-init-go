// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Duration retains Rust's full unsigned-second range, which cannot fit in Go's
// signed nanosecond time.Duration. Nanoseconds must be less than one billion.
type Duration struct {
	Seconds     uint64
	Nanoseconds uint32
}

func DurationFromTime(value time.Duration) (Duration, error) {
	if value < 0 {
		return Duration{}, ErrInvalidDuration
	}
	return Duration{Seconds: uint64(value / time.Second), Nanoseconds: uint32(value % time.Second)}, nil
}

// TimeDuration returns an error rather than overflowing time.Duration.
func (d Duration) TimeDuration() (time.Duration, error) {
	if d.Nanoseconds >= 1e9 || d.Seconds > math.MaxInt64/1_000_000_000 {
		return 0, ErrInvalidDuration
	}
	nanos := d.Seconds*1e9 + uint64(d.Nanoseconds)
	if nanos > math.MaxInt64 {
		return 0, ErrInvalidDuration
	}
	return time.Duration(nanos), nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	if d.Nanoseconds >= 1e9 {
		return nil, ErrInvalidDuration
	}
	return json.Marshal(float64(d.Seconds) + float64(d.Nanoseconds)/1e9)
}

// Precision controls independent timestamp and duration output precision.
type Precision int

const (
	Seconds      Precision = 0
	Milliseconds Precision = 3
	Microseconds Precision = 6
	Nanoseconds  Precision = 9
)

func validPrecision(p Precision) bool {
	return p == Seconds || p == Milliseconds || p == Microseconds || p == Nanoseconds
}

// Format discards fractional digits below precision without rounding.
func (d Duration) Format(precision Precision) (string, error) {
	if d.Nanoseconds >= 1e9 || !validPrecision(precision) {
		return "", ErrInvalidDuration
	}
	whole := strconv.FormatUint(d.Seconds, 10)
	if precision == Seconds {
		return whole, nil
	}
	return whole + "." + fmt.Sprintf("%09d", d.Nanoseconds)[:int(precision)], nil
}

func parseUnsigned(value string) (uint64, error) {
	if value == "" {
		return 0, Malformed
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return 0, Malformed
		}
	}
	number, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, Malformed
	}
	return number, nil
}

func parseDuration(value string) (Duration, error) {
	parts := strings.Split(value, ".")
	if len(parts) <= 2 {
		seconds, err := parseUnsigned(parts[0])
		if err == nil {
			if len(parts) == 1 {
				return Duration{Seconds: seconds}, nil
			}
			fraction := parts[1]
			if len(fraction) > 0 && len(fraction) <= 9 {
				nanos, err := parseUnsigned(fraction)
				if err == nil {
					for i := len(fraction); i < 9; i++ {
						nanos *= 10
					}
					return Duration{Seconds: seconds, Nanoseconds: uint32(nanos)}, nil
				}
			}
		}
	}
	if strings.ContainsAny(value, "_xXpP") {
		return Duration{}, Malformed
	}
	valueFloat, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(valueFloat) || math.IsInf(valueFloat, 0) || valueFloat < 0 {
		return Duration{}, Malformed
	}
	// Round the exact binary float, not an already-rounded float product.
	// For example, f64(5e-10) is slightly above half a nanosecond.
	scaled := new(big.Rat).SetFloat64(valueFloat)
	scaled.Mul(scaled, big.NewRat(1_000_000_000, 1))
	nanos, remainder := new(big.Int), new(big.Int)
	nanos.QuoRem(scaled.Num(), scaled.Denom(), remainder)
	comparison := remainder.Lsh(remainder, 1).Cmp(scaled.Denom())
	if comparison > 0 || (comparison == 0 && nanos.Bit(0) == 1) {
		nanos.Add(nanos, big.NewInt(1))
	}
	seconds := new(big.Int)
	seconds.QuoRem(nanos, big.NewInt(1_000_000_000), remainder)
	if !seconds.IsUint64() {
		return Duration{}, Malformed
	}
	return Duration{Seconds: seconds.Uint64(), Nanoseconds: uint32(remainder.Uint64())}, nil
}

// Timestamp preserves RFC 3339 leap seconds, which time.Time cannot represent.
// For a leap second, Time holds second 59 and LeapSecond marks the extra second.
// Use String for the canonical UTC representation.
type Timestamp struct {
	time.Time
	LeapSecond bool
}

func (t Timestamp) String() string {
	return formatTimestampParts(t.Time, -1, true, t.LeapSecond)
}

func (t Timestamp) MarshalJSON() ([]byte, error) { return json.Marshal(t.String()) }

var timestampPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt ][0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?([Zz]|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

func parseTimestamp(value string) (Timestamp, error) {
	if !timestampPattern.MatchString(value) {
		return Timestamp{}, Malformed
	}
	normalized := []byte(value)
	normalized[10] = 'T'
	if normalized[len(normalized)-1] == 'z' {
		normalized[len(normalized)-1] = 'Z'
	}
	leap := string(normalized[17:19]) == "60"
	if leap {
		normalized[17], normalized[18] = '5', '9'
	}
	parsed, err := time.Parse(time.RFC3339Nano, string(normalized))
	if err != nil {
		return Timestamp{}, Malformed
	}
	return Timestamp{Time: parsed.UTC(), LeapSecond: leap}, nil
}

// digits=-1 selects chrono's automatic 0/3/6/9 fractional digits.
func formatTimestamp(value time.Time, digits Precision, utcZ bool) string {
	return formatTimestampParts(value, digits, utcZ, false)
}

func formatTimestampParts(value time.Time, digits Precision, utcZ, leapSecond bool) string {
	value = value.UTC()
	if digits == -1 {
		switch n := value.Nanosecond(); {
		case n == 0:
			digits = Seconds
		case n%1e6 == 0:
			digits = Milliseconds
		case n%1e3 == 0:
			digits = Microseconds
		default:
			digits = Nanoseconds
		}
	}
	year := fmt.Sprintf("%04d", value.Year())
	if value.Year() < 0 {
		year = fmt.Sprintf("-%04d", -value.Year())
	} else if value.Year() > 9999 {
		year = "+" + year
	}
	second := value.Second()
	if leapSecond {
		second = 60
	}
	result := year + value.Format("-01-02T15:04:") + fmt.Sprintf("%02d", second)
	if digits > 0 {
		result += "." + fmt.Sprintf("%09d", value.Nanosecond())[:int(digits)]
	}
	if utcZ {
		return result + "Z"
	}
	return result + "+00:00"
}
