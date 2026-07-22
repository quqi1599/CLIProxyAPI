package gjson

type Result struct {
	Raw string
}

func (Result) ForEach(iterator func(key, value Result) bool) {}
