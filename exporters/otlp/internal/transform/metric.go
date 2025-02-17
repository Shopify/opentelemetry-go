// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package transform provides translations for opentelemetry-go concepts and
// structures to otlp structures.
package transform

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"go.opentelemetry.io/otel/metric/number"
	export "go.opentelemetry.io/otel/sdk/export/metric"
	"go.opentelemetry.io/otel/sdk/export/metric/aggregation"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
)

var (
	// ErrUnimplementedAgg is returned when a transformation of an unimplemented
	// aggregator is attempted.
	ErrUnimplementedAgg = errors.New("unimplemented aggregator")

	// ErrIncompatibleAgg is returned when
	// aggregation.Kind implies an interface conversion that has
	// failed
	ErrIncompatibleAgg = errors.New("incompatible aggregation type")

	// ErrUnknownValueType is returned when a transformation of an unknown value
	// is attempted.
	ErrUnknownValueType = errors.New("invalid value type")

	// ErrContextCanceled is returned when a context cancellation halts a
	// transformation.
	ErrContextCanceled = errors.New("context canceled")

	// ErrTransforming is returned when an unexected error is encoutered transforming.
	ErrTransforming = errors.New("transforming failed")
)

// result is the product of transforming Records into OTLP Metrics.
type result struct {
	Resource               *resource.Resource
	InstrumentationLibrary instrumentation.Library
	Metric                 *metricpb.Metric
	Err                    error
}

// toNanos returns the number of nanoseconds since the UNIX epoch.
func toNanos(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	return uint64(t.UnixNano())
}

// CheckpointSet transforms all records contained in a checkpoint into
// batched OTLP ResourceMetrics.
func CheckpointSet(ctx context.Context, exportSelector export.ExportKindSelector, cps export.CheckpointSet, numWorkers uint) ([]*metricpb.ResourceMetrics, error) {
	records, errc := source(ctx, exportSelector, cps)

	// Start a fixed number of goroutines to transform records.
	transformed := make(chan result)
	var wg sync.WaitGroup
	wg.Add(int(numWorkers))
	for i := uint(0); i < numWorkers; i++ {
		go func() {
			defer wg.Done()
			transformer(ctx, exportSelector, records, transformed)
		}()
	}
	go func() {
		wg.Wait()
		close(transformed)
	}()

	// Synchronously collect the transformed records and transmit.
	rms, err := sink(ctx, transformed)
	if err != nil {
		return nil, err
	}

	// source is complete, check for any errors.
	if err := <-errc; err != nil {
		return nil, err
	}
	return rms, nil
}

// source starts a goroutine that sends each one of the Records yielded by
// the CheckpointSet on the returned chan. Any error encoutered will be sent
// on the returned error chan after seeding is complete.
func source(ctx context.Context, exportSelector export.ExportKindSelector, cps export.CheckpointSet) (<-chan export.Record, <-chan error) {
	errc := make(chan error, 1)
	out := make(chan export.Record)
	// Seed records into process.
	go func() {
		defer close(out)
		// No select is needed since errc is buffered.
		errc <- cps.ForEach(exportSelector, func(r export.Record) error {
			select {
			case <-ctx.Done():
				return ErrContextCanceled
			case out <- r:
			}
			return nil
		})
	}()
	return out, errc
}

// transformer transforms records read from the passed in chan into
// OTLP Metrics which are sent on the out chan.
func transformer(ctx context.Context, exportSelector export.ExportKindSelector, in <-chan export.Record, out chan<- result) {
	for r := range in {
		m, err := Record(exportSelector, r)
		// Propagate errors, but do not send empty results.
		if err == nil && m == nil {
			continue
		}
		res := result{
			Resource: r.Resource(),
			InstrumentationLibrary: instrumentation.Library{
				Name:    r.Descriptor().InstrumentationName(),
				Version: r.Descriptor().InstrumentationVersion(),
			},
			Metric: m,
			Err:    err,
		}
		select {
		case <-ctx.Done():
			return
		case out <- res:
		}
	}
}

// sink collects transformed Records and batches them.
//
// Any errors encoutered transforming input will be reported with an
// ErrTransforming as well as the completed ResourceMetrics. It is up to the
// caller to handle any incorrect data in these ResourceMetrics.
func sink(ctx context.Context, in <-chan result) ([]*metricpb.ResourceMetrics, error) {
	var errStrings []string

	type resourceBatch struct {
		Resource *resourcepb.Resource
		// Group by instrumentation library name and then the MetricDescriptor.
		InstrumentationLibraryBatches map[instrumentation.Library]map[string]*metricpb.Metric
	}

	// group by unique Resource string.
	grouped := make(map[attribute.Distinct]resourceBatch)
	for res := range in {
		if res.Err != nil {
			errStrings = append(errStrings, res.Err.Error())
			continue
		}

		rID := res.Resource.Equivalent()
		rb, ok := grouped[rID]
		if !ok {
			rb = resourceBatch{
				Resource:                      Resource(res.Resource),
				InstrumentationLibraryBatches: make(map[instrumentation.Library]map[string]*metricpb.Metric),
			}
			grouped[rID] = rb
		}

		mb, ok := rb.InstrumentationLibraryBatches[res.InstrumentationLibrary]
		if !ok {
			mb = make(map[string]*metricpb.Metric)
			rb.InstrumentationLibraryBatches[res.InstrumentationLibrary] = mb
		}

		mID := res.Metric.GetName()
		m, ok := mb[mID]
		if !ok {
			mb[mID] = res.Metric
			continue
		}
		switch res.Metric.Data.(type) {
		case *metricpb.Metric_Gauge:
			m.GetGauge().DataPoints = append(m.GetGauge().DataPoints, res.Metric.GetGauge().DataPoints...)
		case *metricpb.Metric_Sum:
			m.GetSum().DataPoints = append(m.GetSum().DataPoints, res.Metric.GetSum().DataPoints...)
		case *metricpb.Metric_Histogram:
			m.GetHistogram().DataPoints = append(m.GetHistogram().DataPoints, res.Metric.GetHistogram().DataPoints...)
		case *metricpb.Metric_Summary:
			m.GetSummary().DataPoints = append(m.GetSummary().DataPoints, res.Metric.GetSummary().DataPoints...)
		default:
			err := fmt.Sprintf("unsupported metric type: %T", res.Metric.Data)
			errStrings = append(errStrings, err)
		}
	}

	if len(grouped) == 0 {
		return nil, nil
	}

	var rms []*metricpb.ResourceMetrics
	for _, rb := range grouped {
		rm := &metricpb.ResourceMetrics{Resource: rb.Resource}
		for il, mb := range rb.InstrumentationLibraryBatches {
			ilm := &metricpb.InstrumentationLibraryMetrics{
				Metrics: make([]*metricpb.Metric, 0, len(mb)),
			}
			if il != (instrumentation.Library{}) {
				ilm.InstrumentationLibrary = &commonpb.InstrumentationLibrary{
					Name:    il.Name,
					Version: il.Version,
				}
			}
			for _, m := range mb {
				ilm.Metrics = append(ilm.Metrics, m)
			}
			rm.InstrumentationLibraryMetrics = append(rm.InstrumentationLibraryMetrics, ilm)
		}
		rms = append(rms, rm)
	}

	// Report any transform errors.
	if len(errStrings) > 0 {
		return rms, fmt.Errorf("%w:\n -%s", ErrTransforming, strings.Join(errStrings, "\n -"))
	}
	return rms, nil
}

// Record transforms a Record into an OTLP Metric. An ErrIncompatibleAgg
// error is returned if the Record Aggregator is not supported.
func Record(exportSelector export.ExportKindSelector, r export.Record) (*metricpb.Metric, error) {
	agg := r.Aggregation()
	switch agg.Kind() {
	case aggregation.MinMaxSumCountKind:
		mmsc, ok := agg.(aggregation.MinMaxSumCount)
		if !ok {
			return nil, fmt.Errorf("%w: %T", ErrIncompatibleAgg, agg)
		}
		return minMaxSumCount(r, mmsc)

	case aggregation.HistogramKind:
		h, ok := agg.(aggregation.Histogram)
		if !ok {
			return nil, fmt.Errorf("%w: %T", ErrIncompatibleAgg, agg)
		}
		return histogramPoint(r, exportSelector.ExportKindFor(r.Descriptor(), aggregation.HistogramKind), h)

	case aggregation.SumKind:
		s, ok := agg.(aggregation.Sum)
		if !ok {
			return nil, fmt.Errorf("%w: %T", ErrIncompatibleAgg, agg)
		}
		sum, err := s.Sum()
		if err != nil {
			return nil, err
		}
		return sumPoint(r, sum, r.StartTime(), r.EndTime(), exportSelector.ExportKindFor(r.Descriptor(), aggregation.SumKind), r.Descriptor().InstrumentKind().Monotonic())

	case aggregation.LastValueKind:
		lv, ok := agg.(aggregation.LastValue)
		if !ok {
			return nil, fmt.Errorf("%w: %T", ErrIncompatibleAgg, agg)
		}
		value, tm, err := lv.LastValue()
		if err != nil {
			return nil, err
		}
		return gaugePoint(r, value, time.Time{}, tm)

	case aggregation.ExactKind:
		e, ok := agg.(aggregation.Points)
		if !ok {
			return nil, fmt.Errorf("%w: %T", ErrIncompatibleAgg, agg)
		}
		pts, err := e.Points()
		if err != nil {
			return nil, err
		}

		return gaugeArray(r, pts)

	default:
		return nil, fmt.Errorf("%w: %T", ErrUnimplementedAgg, agg)
	}
}

func gaugeArray(record export.Record, points []aggregation.Point) (*metricpb.Metric, error) {
	desc := record.Descriptor()
	labels := record.Labels()
	m := &metricpb.Metric{
		Name:        desc.Name(),
		Description: desc.Description(),
		Unit:        string(desc.Unit()),
	}

	pbAttrs := keyValues(labels.Iter())

	ndp := make([]*metricpb.NumberDataPoint, 0, len(points))
	switch nk := desc.NumberKind(); nk {
	case number.Int64Kind:
		for _, p := range points {
			ndp = append(ndp, &metricpb.NumberDataPoint{
				Attributes:        pbAttrs,
				StartTimeUnixNano: toNanos(record.StartTime()),
				TimeUnixNano:      toNanos(record.EndTime()),
				Value: &metricpb.NumberDataPoint_AsInt{
					AsInt: p.Number.CoerceToInt64(nk),
				},
			})
		}
	case number.Float64Kind:
		for _, p := range points {
			ndp = append(ndp, &metricpb.NumberDataPoint{
				Attributes:        pbAttrs,
				StartTimeUnixNano: toNanos(record.StartTime()),
				TimeUnixNano:      toNanos(record.EndTime()),
				Value: &metricpb.NumberDataPoint_AsDouble{
					AsDouble: p.Number.CoerceToFloat64(nk),
				},
			})
		}
	default:
		return nil, fmt.Errorf("%w: %v", ErrUnknownValueType, nk)
	}

	m.Data = &metricpb.Metric_Gauge{
		Gauge: &metricpb.Gauge{
			DataPoints: ndp,
		},
	}
	return m, nil
}

func gaugePoint(record export.Record, num number.Number, start, end time.Time) (*metricpb.Metric, error) {
	desc := record.Descriptor()
	labels := record.Labels()

	m := &metricpb.Metric{
		Name:        desc.Name(),
		Description: desc.Description(),
		Unit:        string(desc.Unit()),
	}

	switch n := desc.NumberKind(); n {
	case number.Int64Kind:
		m.Data = &metricpb.Metric_Gauge{
			Gauge: &metricpb.Gauge{
				DataPoints: []*metricpb.NumberDataPoint{
					{
						Value: &metricpb.NumberDataPoint_AsInt{
							AsInt: num.CoerceToInt64(n),
						},
						Attributes:        keyValues(labels.Iter()),
						StartTimeUnixNano: toNanos(start),
						TimeUnixNano:      toNanos(end),
					},
				},
			},
		}
	case number.Float64Kind:
		m.Data = &metricpb.Metric_Gauge{
			Gauge: &metricpb.Gauge{
				DataPoints: []*metricpb.NumberDataPoint{
					{
						Value: &metricpb.NumberDataPoint_AsDouble{
							AsDouble: num.CoerceToFloat64(n),
						},
						Attributes:        keyValues(labels.Iter()),
						StartTimeUnixNano: toNanos(start),
						TimeUnixNano:      toNanos(end),
					},
				},
			},
		}
	default:
		return nil, fmt.Errorf("%w: %v", ErrUnknownValueType, n)
	}

	return m, nil
}

func exportKindToTemporality(ek export.ExportKind) metricpb.AggregationTemporality {
	switch ek {
	case export.DeltaExportKind:
		return metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
	case export.CumulativeExportKind:
		return metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
	}
	return metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_UNSPECIFIED
}

func sumPoint(record export.Record, num number.Number, start, end time.Time, ek export.ExportKind, monotonic bool) (*metricpb.Metric, error) {
	desc := record.Descriptor()
	labels := record.Labels()

	m := &metricpb.Metric{
		Name:        desc.Name(),
		Description: desc.Description(),
		Unit:        string(desc.Unit()),
	}

	switch n := desc.NumberKind(); n {
	case number.Int64Kind:
		m.Data = &metricpb.Metric_Sum{
			Sum: &metricpb.Sum{
				IsMonotonic:            monotonic,
				AggregationTemporality: exportKindToTemporality(ek),
				DataPoints: []*metricpb.NumberDataPoint{
					{
						Value: &metricpb.NumberDataPoint_AsInt{
							AsInt: num.CoerceToInt64(n),
						},
						Attributes:        keyValues(labels.Iter()),
						StartTimeUnixNano: toNanos(start),
						TimeUnixNano:      toNanos(end),
					},
				},
			},
		}
	case number.Float64Kind:
		m.Data = &metricpb.Metric_Sum{
			Sum: &metricpb.Sum{
				IsMonotonic:            monotonic,
				AggregationTemporality: exportKindToTemporality(ek),
				DataPoints: []*metricpb.NumberDataPoint{
					{
						Value: &metricpb.NumberDataPoint_AsDouble{
							AsDouble: num.CoerceToFloat64(n),
						},
						Attributes:        keyValues(labels.Iter()),
						StartTimeUnixNano: toNanos(start),
						TimeUnixNano:      toNanos(end),
					},
				},
			},
		}
	default:
		return nil, fmt.Errorf("%w: %v", ErrUnknownValueType, n)
	}

	return m, nil
}

// minMaxSumCountValue returns the values of the MinMaxSumCount Aggregator
// as discrete values.
func minMaxSumCountValues(a aggregation.MinMaxSumCount) (min, max, sum number.Number, count uint64, err error) {
	if min, err = a.Min(); err != nil {
		return
	}
	if max, err = a.Max(); err != nil {
		return
	}
	if sum, err = a.Sum(); err != nil {
		return
	}
	if count, err = a.Count(); err != nil {
		return
	}
	return
}

// minMaxSumCount transforms a MinMaxSumCount Aggregator into an OTLP Metric.
func minMaxSumCount(record export.Record, a aggregation.MinMaxSumCount) (*metricpb.Metric, error) {
	desc := record.Descriptor()
	labels := record.Labels()
	min, max, sum, count, err := minMaxSumCountValues(a)
	if err != nil {
		return nil, err
	}

	m := &metricpb.Metric{
		Name:        desc.Name(),
		Description: desc.Description(),
		Unit:        string(desc.Unit()),
		Data: &metricpb.Metric_Summary{
			Summary: &metricpb.Summary{
				DataPoints: []*metricpb.SummaryDataPoint{
					{
						Sum:               sum.CoerceToFloat64(desc.NumberKind()),
						Attributes:        keyValues(labels.Iter()),
						StartTimeUnixNano: toNanos(record.StartTime()),
						TimeUnixNano:      toNanos(record.EndTime()),
						Count:             uint64(count),
						QuantileValues: []*metricpb.SummaryDataPoint_ValueAtQuantile{
							{
								Quantile: 0.0,
								Value:    min.CoerceToFloat64(desc.NumberKind()),
							},
							{
								Quantile: 1.0,
								Value:    max.CoerceToFloat64(desc.NumberKind()),
							},
						},
					},
				},
			},
		},
	}
	return m, nil
}

func histogramValues(a aggregation.Histogram) (boundaries []float64, counts []uint64, err error) {
	var buckets aggregation.Buckets
	if buckets, err = a.Histogram(); err != nil {
		return
	}
	boundaries, counts = buckets.Boundaries, buckets.Counts
	if len(counts) != len(boundaries)+1 {
		err = ErrTransforming
		return
	}
	return
}

// histogram transforms a Histogram Aggregator into an OTLP Metric.
func histogramPoint(record export.Record, ek export.ExportKind, a aggregation.Histogram) (*metricpb.Metric, error) {
	desc := record.Descriptor()
	labels := record.Labels()
	boundaries, counts, err := histogramValues(a)
	if err != nil {
		return nil, err
	}

	count, err := a.Count()
	if err != nil {
		return nil, err
	}

	sum, err := a.Sum()
	if err != nil {
		return nil, err
	}

	m := &metricpb.Metric{
		Name:        desc.Name(),
		Description: desc.Description(),
		Unit:        string(desc.Unit()),
		Data: &metricpb.Metric_Histogram{
			Histogram: &metricpb.Histogram{
				AggregationTemporality: exportKindToTemporality(ek),
				DataPoints: []*metricpb.HistogramDataPoint{
					{
						Sum:               sum.CoerceToFloat64(desc.NumberKind()),
						Attributes:        keyValues(labels.Iter()),
						StartTimeUnixNano: toNanos(record.StartTime()),
						TimeUnixNano:      toNanos(record.EndTime()),
						Count:             uint64(count),
						BucketCounts:      counts,
						ExplicitBounds:    boundaries,
					},
				},
			},
		},
	}
	return m, nil
}

// keyValues transforms an attribute iterator into an OTLP KeyValues.
func keyValues(iter attribute.Iterator) []*commonpb.KeyValue {
	l := iter.Len()
	if l == 0 {
		return nil
	}
	result := make([]*commonpb.KeyValue, 0, l)
	for iter.Next() {
		kv := iter.Label()
		result = append(result, &commonpb.KeyValue{
			Key:   string(kv.Key),
			Value: value(kv.Value),
		})
	}
	return result
}

// value transforms an attribute Value into an OTLP AnyValue.
func value(v attribute.Value) *commonpb.AnyValue {
	switch v.Type() {
	case attribute.BOOL:
		return &commonpb.AnyValue{
			Value: &commonpb.AnyValue_BoolValue{
				BoolValue: v.AsBool(),
			},
		}
	case attribute.INT64:
		return &commonpb.AnyValue{
			Value: &commonpb.AnyValue_IntValue{
				IntValue: v.AsInt64(),
			},
		}
	case attribute.FLOAT64:
		return &commonpb.AnyValue{
			Value: &commonpb.AnyValue_DoubleValue{
				DoubleValue: v.AsFloat64(),
			},
		}
	case attribute.ARRAY:
		return &commonpb.AnyValue{
			Value: &commonpb.AnyValue_ArrayValue{
				ArrayValue: &commonpb.ArrayValue{
					Values: arrayValue(v.AsArray()),
				},
			},
		}
	default:
		return &commonpb.AnyValue{
			Value: &commonpb.AnyValue_StringValue{
				StringValue: v.Emit(),
			},
		}
	}
}

// arrayValue transforms an attribute Value of ARRAY type into an slice of
// OTLP AnyValue.
func arrayValue(arr interface{}) []*commonpb.AnyValue {
	var av []*commonpb.AnyValue
	switch val := arr.(type) {
	case []bool:
		av = make([]*commonpb.AnyValue, len(val))
		for i, v := range val {
			av[i] = &commonpb.AnyValue{
				Value: &commonpb.AnyValue_BoolValue{
					BoolValue: v,
				},
			}
		}
	case []int:
		av = make([]*commonpb.AnyValue, len(val))
		for i, v := range val {
			av[i] = &commonpb.AnyValue{
				Value: &commonpb.AnyValue_IntValue{
					IntValue: int64(v),
				},
			}
		}
	case []int64:
		av = make([]*commonpb.AnyValue, len(val))
		for i, v := range val {
			av[i] = &commonpb.AnyValue{
				Value: &commonpb.AnyValue_IntValue{
					IntValue: v,
				},
			}
		}
	case []float64:
		av = make([]*commonpb.AnyValue, len(val))
		for i, v := range val {
			av[i] = &commonpb.AnyValue{
				Value: &commonpb.AnyValue_DoubleValue{
					DoubleValue: v,
				},
			}
		}
	case []string:
		av = make([]*commonpb.AnyValue, len(val))
		for i, v := range val {
			av[i] = &commonpb.AnyValue{
				Value: &commonpb.AnyValue_StringValue{
					StringValue: v,
				},
			}
		}
	}
	return av
}
