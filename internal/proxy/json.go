// json.go — small helper kept out of proxy.go to keep its import list clean.
package proxy

import "encoding/json"

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
