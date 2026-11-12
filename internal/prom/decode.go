package prom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/glapsfun/sre-agent/internal/safety"
)

// A matrix has many small series. Bound each conversion independently so the
// discovery decoder's work limit does not become a whole-matrix sample limit.
func decodeResponse(ctx context.Context, body []byte, out *apiResponse) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(body) > safety.MaxResponseBytes {
		return safety.ErrResponseTooLarge
	}
	d := responseDecoder{ctx: ctx, decoder: json.NewDecoder(&responseReader{ctx: ctx, reader: bytes.NewReader(body)})}
	present, err := d.object(func(key string) error {
		switch key {
		case "status":
			return d.field(&out.Status)
		case "error":
			return d.field(&out.Error)
		case "errorType":
			return d.field(&out.ErrorType)
		case "warnings":
			return d.field(&out.Warnings)
		case "infos":
			return d.field(&out.Infos)
		case "data":
			data := new(apiData)
			present, err := d.object(func(key string) error {
				switch key {
				case "resultType":
					return d.field(&data.ResultType)
				case "result":
					return d.series(&data.Result)
				default:
					return d.skip()
				}
			})
			if present {
				out.Data = data
			} else {
				out.Data = nil
			}
			return err
		default:
			return d.skip()
		}
	})
	if err != nil {
		return err
	}
	if !present {
		return errors.New("expected Prometheus response object")
	}
	if _, err = d.decoder.Token(); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("multiple JSON values")
	}
	return ctx.Err()
}

type responseReader struct {
	ctx    context.Context
	reader *bytes.Reader
}

func (r *responseReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > 4096 {
		p = p[:4096]
	}
	return r.reader.Read(p)
}

type responseDecoder struct {
	ctx     context.Context
	decoder *json.Decoder
}

func (d responseDecoder) object(visit func(string) error) (bool, error) {
	token, err := d.decoder.Token()
	if err != nil {
		return false, err
	}
	if token == nil {
		return false, nil
	}
	if token != json.Delim('{') {
		return false, errors.New("expected JSON object")
	}
	for d.decoder.More() {
		if err := d.ctx.Err(); err != nil {
			return false, err
		}
		token, err := d.decoder.Token()
		if err != nil {
			return false, err
		}
		key, ok := token.(string)
		if !ok {
			return false, errors.New("expected JSON field name")
		}
		if err = visit(key); err != nil {
			return false, err
		}
	}
	_, err = d.decoder.Token()
	return true, err
}

func (d responseDecoder) field(dest any) error {
	var raw json.RawMessage
	if err := d.decoder.Decode(&raw); err != nil {
		return err
	}
	return safety.DecodeJSON(d.ctx, string(raw), dest)
}

func (d responseDecoder) skip() error {
	var raw json.RawMessage
	return d.field(&raw)
}

func (d responseDecoder) series(out *[]apiSeries) error {
	token, err := d.decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		*out = nil
		return nil
	}
	if token != json.Delim('[') {
		return errors.New("expected result array")
	}
	*out = []apiSeries{}
	for d.decoder.More() {
		if err := d.ctx.Err(); err != nil {
			return err
		}
		var row apiSeries
		if err := d.field(&row); err != nil {
			return err
		}
		*out = append(*out, row)
	}
	_, err = d.decoder.Token()
	return err
}
