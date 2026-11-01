package capture

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

func decodeQueries(data []byte, queries *[]telemetry.Query) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(queries); err != nil {
		return errors.New("invalid query JSON")
	}
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("trailing query JSON")
	}
	return nil
}
