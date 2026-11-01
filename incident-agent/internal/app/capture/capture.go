// Package capture collects a finite set of telemetry queries into frozen evidence.
package capture

import (
	"context"
	"crypto/rand"
	"errors"
	"io"

	"github.com/hughtaiyaki/incident-agent/internal/telemetry"
)

// Querier is the consumer-owned boundary shared by online adapters and offline replay.
type Querier interface {
	Query(context.Context, telemetry.Query, *telemetry.Budget) (telemetry.Result, error)
}

// ReadQueries rejects unknown fields, oversized input and invalid queries before collection.
func ReadQueries(reader io.Reader, policy telemetry.Policy) ([]telemetry.Query, error) {
	data, err := io.ReadAll(io.LimitReader(reader, 16*1024+1))
	if err != nil {
		return nil, errors.New("cannot read queries")
	}
	if len(data) > 16*1024 {
		return nil, telemetry.ErrTooLarge
	}
	var queries []telemetry.Query
	if err = decodeQueries(data, &queries); err != nil {
		return nil, err
	}
	if len(queries) == 0 || len(queries) > 16 {
		return nil, errors.New("expected one to sixteen queries")
	}
	for i, q := range queries {
		queries[i], err = policy.Normalize(q)
		if err != nil {
			return nil, err
		}
	}
	return queries, nil
}

// Collect completes independent queries even when a source is unavailable, preserving failure evidence.
func Collect(ctx context.Context, querier Querier, queries []telemetry.Query) ([]telemetry.Result, error) {
	results := []telemetry.Result{}
	budget := telemetry.NewBudget(16)
	failures := []error{}
	for _, query := range queries {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		result, err := querier.Query(ctx, query, budget)
		if err != nil {
			failures = append(failures, err)
		}
		if result.Source != "" {
			results = append(results, result)
		}
	}
	return results, errors.Join(failures...)
}

// Freeze anonymizes correlation IDs before the bundle can be saved or shared.
func Freeze(origin, upstreamSHA string, entries []telemetry.Result) (telemetry.Bundle, error) {
	bundle, err := telemetry.NewBundle(origin, upstreamSHA, entries)
	if err != nil {
		return telemetry.Bundle{}, err
	}
	salt := make([]byte, 32)
	if _, err = rand.Read(salt); err != nil {
		return telemetry.Bundle{}, errors.New("cannot initialize anonymization")
	}
	if err = bundle.Anonymize(salt); err != nil {
		return telemetry.Bundle{}, err
	}
	return bundle, nil
}
