package helps

import (
	"strconv"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// NormalizeCodexRequestSchemas sanitizes function schemas without touching tool data.
func NormalizeCodexRequestSchemas(body []byte) []byte {
	var visit func(gjson.Result, string)
	visit = func(tools gjson.Result, prefix string) {
		if !tools.IsArray() {
			return
		}
		for index, tool := range tools.Array() {
			path := prefix + "." + strconv.Itoa(index)
			switch tool.Get("type").String() {
			case "namespace":
				visit(tool.Get("tools"), path+".tools")
			case "function":
				params := tool.Get("parameters")
				if params.IsObject() {
					if updated, err := sjson.SetRawBytes(body, path+".parameters", util.NormalizeCodexToolParameters([]byte(params.Raw))); err == nil {
						body = updated
					}
				}
			}
		}
	}
	visit(gjson.GetBytes(body, "tools"), "tools")
	return body
}
