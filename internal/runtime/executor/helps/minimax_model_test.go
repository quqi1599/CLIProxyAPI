package helps

import "testing"

func TestOfficialMiniMaxM31ModelNamesAndEndpointBoundaries(t *testing.T) {
	for _, base := range []string{"https://api.minimaxi.com/v1", "https://api.minimax.cn/anthropic", "https://api.minimax.io/anthropic/v1", "https://API.MINIMAX.IO:443/"} {
		for _, name := range []string{"MiniMax-M3.1", "MiniMax-M3.1-flash", "MiniMax-M3.1-Flash-Preview"} {
			if got := OfficialMiniMaxModel(name, base); got != MiniMaxM31OfficialModel {
				t.Fatal(base, name, got)
			}
			if got := OfficialMiniMaxModel(name+"(low)", base); got != MiniMaxM31OfficialModel+"(low)" {
				t.Fatal("effort suffix lost", got)
			}
		}
	}
	for _, base := range []string{"http://api.minimaxi.com/v1", "https://api.minimaxi.com:444/v1", "https://api.minimaxi.com.evil/v1", "https://fixture.invalid/v1", "https://api.minimaxi.com/proxy/v1", "https://api.minimaxi.com/v1?token=private", "https://user:private@api.minimaxi.com/v1"} {
		if got := OfficialMiniMaxModel("MiniMax-M3.1", base); got != "MiniMax-M3.1" {
			t.Fatal("custom endpoint model rewritten", base)
		}
	}
	for _, model := range []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M3.10", "MiniMax-M3.1-image", "private/MiniMax-M3.1"} {
		if got := OfficialMiniMaxModel(model, "https://api.minimaxi.com/v1"); got != model {
			t.Fatal("unrelated model rewritten", model)
		}
	}
}
