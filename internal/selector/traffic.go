package selector

import (
	"fmt"
	"strings"
)

// Traffic is the class of a request: from outside the cluster through a
// gateway (north-south) or between workloads inside it (east-west).
type Traffic uint8

const (
	// TrafficAny matches both classes. As a request class it means "unknown".
	TrafficAny Traffic = iota
	TrafficNorthSouth
	TrafficEastWest
)

// TrafficMetadataKey is the ext_proc gRPC metadata key that carries the class.
// It is set by the operator-owned Envoy filter configuration, never by clients.
const TrafficMetadataKey = "x-hypergate-traffic"

func (t Traffic) String() string {
	switch t {
	case TrafficNorthSouth:
		return "north_south"
	case TrafficEastWest:
		return "east_west"
	default:
		return "any"
	}
}

// ParseTraffic accepts north_south, east_west and any (case-insensitive, '-'
// and '_' interchangeable, so "north-south" and "NorthSouth" are accepted too).
// An empty value means any.
func ParseTraffic(v string) (Traffic, error) {
	n := strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(strings.TrimSpace(v)))
	switch n {
	case "", "any":
		return TrafficAny, nil
	case "northsouth":
		return TrafficNorthSouth, nil
	case "eastwest":
		return TrafficEastWest, nil
	}
	return TrafficAny, fmt.Errorf("invalid traffic %q (expected north_south, east_west or any)", v)
}
