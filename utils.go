package lightauth

import (
	"net/http"
)

func readHeader(h http.Header, header string) string {
	_value, headerExists := h[header]
	var value string
	if !headerExists {
		value = ""
	} else {
		value = _value[0]
	}

	return value
}
