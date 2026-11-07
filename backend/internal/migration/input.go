package migration

import (
	"math"
	"net/http"
	"net/url"

	"github.com/alfarabi0642/IF3110-DataBrainManagementSystem/backend/internal/httpx"
	"github.com/google/uuid"
)

func draftPathID(r *http.Request, name string) (uuid.UUID, error) {
	return httpx.UUIDValue(url.Values{name: {r.PathValue(name)}}, name)
}

func draftPagination(r *http.Request) (int, int, error) {
	values, err := httpx.ParseQuery(r, "limit", "offset")
	if err != nil {
		return 0, 0, err
	}
	limit, offset := int64(50), int64(0)
	if values.Has("limit") {
		limit, err = httpx.Int64Value(values, "limit", 1, 200)
	}
	if err == nil && values.Has("offset") {
		offset, err = httpx.Int64Value(values, "offset", 0, 100000)
	}
	return int(limit), int(offset), err
}

func reviewInput(values url.Values) (ReserveInput, error) {
	revision, err := httpx.Int64Value(values, "expected_revision", 1, math.MaxInt64)
	if err != nil {
		return ReserveInput{}, err
	}
	key, err := httpx.UUIDValue(values, "request_key")
	return ReserveInput{revision, key}, err
}
