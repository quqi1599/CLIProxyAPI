package a

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/tidwall/sjson"
)

func appendRaw(body []byte, values [][]byte) []byte {
	for _, value := range values {
		body, _ = sjson.SetRawBytes(body, "output.-1", value) // want "PG001 payload growth risk"
	}
	return body
}

func dynamicRawPath(body []byte, values [][]byte, path string) []byte {
	for _, value := range values {
		body, _ = sjson.SetRawBytes(body, path, value) // want "PG002 payload growth risk"
	}
	return body
}

func constructedAppendPath(body []byte, values [][]byte) []byte {
	var path string
	for _, value := range values {
		path = "output." + "-1"
		body, _ = sjson.SetRawBytes(body, path, value) // want "PG001 payload growth risk"
	}
	return body
}

func formattedAppendPath(body []byte, values [][]byte) []byte {
	for index, value := range values {
		path := fmt.Sprintf("output.%d", -1)
		if index%2 == 0 {
			path = "output." + strconv.Itoa(-1)
		}
		body, _ = sjson.SetRawBytes(body, path, value) // want "PG001 payload growth risk"
	}
	return body
}

func rewriteBody(body []byte, values []string) []byte {
	for _, value := range values {
		body, _ = sjson.SetBytes(body, "model", value) // want "PG002 payload growth risk"
	}
	return body
}

func rewriteBodyThroughAlias(body []byte, values []string) []byte {
	for _, value := range values {
		updated, _ := sjson.SetBytes(body, "model", value) // want "PG002 payload growth risk"
		body = updated
	}
	return body
}

func rewriteStringThroughConversion(body string, values []string) string {
	for _, value := range values {
		updated, _ := sjson.SetBytes([]byte(body), "model", value) // want "PG002 payload growth risk"
		body = string(updated)
	}
	return body
}

func rewriteThroughFullSlice(body []byte, values []string) []byte {
	for _, value := range values {
		updated, _ := sjson.SetBytes(body, "model", value) // want "PG002 payload growth risk"
		body = updated[:]
	}
	return body
}

func rewriteThroughClone(body []byte, values []string) []byte {
	for _, value := range values {
		updated, _ := sjson.SetBytes(body, "model", value) // want "PG002 payload growth risk"
		body = bytes.Clone(updated)
	}
	return body
}

type payloadHolder struct {
	body []byte
}

func rewriteField(holder *payloadHolder, values []string) {
	for _, value := range values {
		holder.body, _ = sjson.SetBytes(holder.body, "model", value) // want "PG002 payload growth risk"
	}
}

func marshalGrowingSlice(values []string) []byte {
	var items []string
	var output []byte
	for _, value := range values {
		items = append(items, value)
		output, _ = json.Marshal(items) // want "PG003 payload growth risk"
	}
	return output
}

func marshalGrowingMap(values []string) []byte {
	items := make(map[string]string)
	var output []byte
	for _, value := range values {
		items[value] = value
		output, _ = json.Marshal(items) // want "PG003 payload growth risk"
	}
	return output
}

func marshalGrowingSlicePointer(values []string) []byte {
	var items []string
	var output []byte
	for _, value := range values {
		items = append(items, value)
		output, _ = json.Marshal(&items) // want "PG003 payload growth risk"
	}
	return output
}

func justifiedAppend(body []byte, values [][]byte) []byte {
	for _, value := range values {
		//nolint:payload-growth benchmark=BenchmarkPayloadGrowthJustified upstream API requires incremental patches
		body, _ = sjson.SetRawBytes(body, "output.-1", value)
	}
	return body
}

func justifiedClassicAppend(body []byte, values [][]byte) []byte {
	for _, value := range values {
		//nolint:payload-growth benchmark=BenchmarkPayloadGrowthClassic standard benchmark loop covers this exception
		body, _ = sjson.SetRawBytes(body, "output.-1", value)
	}
	return body
}

func missingBenchmark(body []byte, values [][]byte) []byte {
	for _, value := range values {
		//nolint:payload-growth benchmark=BenchmarkPayloadGrowthMissing legacy contract
		body, _ = sjson.SetRawBytes(body, "output.-1", value) // want "PG001 payload growth risk"
	}
	return body
}

func ignoredBenchmark(body []byte, values [][]byte) []byte {
	for _, value := range values {
		//nolint:payload-growth benchmark=BenchmarkPayloadGrowthIgnored ignored file must not unlock suppression
		body, _ = sjson.SetRawBytes(body, "output.-1", value) // want "PG001 payload growth risk"
	}
	return body
}

func skippedBenchmark(body []byte, values [][]byte) []byte {
	for _, value := range values {
		//nolint:payload-growth benchmark=BenchmarkPayloadGrowthSkipped skipped benchmark must not unlock suppression
		body, _ = sjson.SetRawBytes(body, "output.-1", value) // want "PG001 payload growth risk"
	}
	return body
}

func outsideLoopBenchmark(body []byte, values [][]byte) []byte {
	for _, value := range values {
		//nolint:payload-growth benchmark=BenchmarkPayloadGrowthOutsideLoop benchmark must exercise target in measured loop
		body, _ = sjson.SetRawBytes(body, "output.-1", value) // want "PG001 payload growth risk"
	}
	return body
}

func shadowedBenchmark(body []byte, values [][]byte) []byte {
	for _, value := range values {
		//nolint:payload-growth benchmark=BenchmarkPayloadGrowthShadowed same-name closure is not evidence
		body, _ = sjson.SetRawBytes(body, "output.-1", value) // want "PG001 payload growth risk"
	}
	return body
}

func brokenLoopBenchmark(body []byte, values [][]byte) []byte {
	for _, value := range values {
		//nolint:payload-growth benchmark=BenchmarkPayloadGrowthBrokenLoop noncanonical loop is not evidence
		body, _ = sjson.SetRawBytes(body, "output.-1", value) // want "PG001 payload growth risk"
	}
	return body
}

func unreachableBenchmark(body []byte, values [][]byte) []byte {
	for _, value := range values {
		//nolint:payload-growth benchmark=BenchmarkPayloadGrowthUnreachable unreachable call is not evidence
		body, _ = sjson.SetRawBytes(body, "output.-1", value) // want "PG001 payload growth risk"
	}
	return body
}

func earlyExitBenchmark(body []byte, values [][]byte) []byte {
	for _, value := range values {
		//nolint:payload-growth benchmark=BenchmarkPayloadGrowthEarlyExit benchmark must measure every iteration
		body, _ = sjson.SetRawBytes(body, "output.-1", value) // want "PG001 payload growth risk"
	}
	return body
}

func safeBuild(values [][]byte) []byte {
	items := make([][]byte, 0, len(values))
	for _, value := range values {
		items = append(items, value)
	}
	output, _ := json.Marshal(items)
	return output
}

func safePerIterationMarshal(values []string) []byte {
	var items []string
	var output []byte
	for _, value := range values {
		items = append([]string(nil), value)
		output, _ = json.Marshal(items)
	}
	return output
}

func safeResetBeforeMarshal(values []string) []byte {
	var items []string
	var output []byte
	for _, value := range values {
		items = items[:0]
		items = append(items, value)
		output, _ = json.Marshal(items)
	}
	return output
}

func safeFixedMapKey(values []string) []byte {
	items := make(map[string]string)
	var output []byte
	for _, value := range values {
		items["value"] = value
		output, _ = json.Marshal(items)
	}
	return output
}

func conditionalResetDoesNotProveSafety(values []string) []byte {
	var items []string
	var output []byte
	for _, value := range values {
		if false {
			clear(items)
		}
		items = append(items, value)
		output, _ = json.Marshal(items) // want "PG003 payload growth risk"
	}
	return output
}

func safeMakeBeforeMarshal(values []string) []byte {
	var items []string
	var output []byte
	for _, value := range values {
		items = make([]string, 0, 1)
		items = append(items, value)
		output, _ = json.Marshal(items)
	}
	return output
}
