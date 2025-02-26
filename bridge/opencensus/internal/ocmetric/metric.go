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

package internal // import "go.opentelemetry.io/otel/bridge/opencensus/internal/ocmetric"

import (
	"errors"
	"fmt"

	ocmetricdata "go.opencensus.io/metric/metricdata"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var (
	errAggregationType              = errors.New("unsupported OpenCensus aggregation type")
	errMismatchedValueTypes         = errors.New("wrong value type for data point")
	errNegativeDistributionCount    = errors.New("distribution count is negative")
	errNegativeBucketCount          = errors.New("distribution bucket count is negative")
	errMismatchedAttributeKeyValues = errors.New("mismatched number of attribute keys and values")
)

// ConvertMetrics converts metric data from OpenCensus to OpenTelemetry.
func ConvertMetrics(ocmetrics []*ocmetricdata.Metric) ([]metricdata.Metrics, error) {
	otelMetrics := make([]metricdata.Metrics, 0, len(ocmetrics))
	var err error
	for _, ocm := range ocmetrics {
		if ocm == nil {
			continue
		}
		agg, aggregationErr := convertAggregation(ocm)
		if aggregationErr != nil {
			err = errors.Join(err, fmt.Errorf("error converting metric %v: %w", ocm.Descriptor.Name, aggregationErr))
			continue
		}
		otelMetrics = append(otelMetrics, metricdata.Metrics{
			Name:        ocm.Descriptor.Name,
			Description: ocm.Descriptor.Description,
			Unit:        string(ocm.Descriptor.Unit),
			Data:        agg,
		})
	}
	if err != nil {
		return otelMetrics, fmt.Errorf("error converting from OpenCensus to OpenTelemetry: %w", err)
	}
	return otelMetrics, nil
}

// convertAggregation produces an aggregation based on the OpenCensus Metric.
func convertAggregation(metric *ocmetricdata.Metric) (metricdata.Aggregation, error) {
	labelKeys := metric.Descriptor.LabelKeys
	switch metric.Descriptor.Type {
	case ocmetricdata.TypeGaugeInt64:
		return convertGauge[int64](labelKeys, metric.TimeSeries)
	case ocmetricdata.TypeGaugeFloat64:
		return convertGauge[float64](labelKeys, metric.TimeSeries)
	case ocmetricdata.TypeCumulativeInt64:
		return convertSum[int64](labelKeys, metric.TimeSeries)
	case ocmetricdata.TypeCumulativeFloat64:
		return convertSum[float64](labelKeys, metric.TimeSeries)
	case ocmetricdata.TypeCumulativeDistribution:
		return convertHistogram(labelKeys, metric.TimeSeries)
		// TODO: Support summaries, once it is in the OTel data types.
	}
	return nil, fmt.Errorf("%w: %q", errAggregationType, metric.Descriptor.Type)
}

// convertGauge converts an OpenCensus gauge to an OpenTelemetry gauge aggregation.
func convertGauge[N int64 | float64](labelKeys []ocmetricdata.LabelKey, ts []*ocmetricdata.TimeSeries) (metricdata.Gauge[N], error) {
	points, err := convertNumberDataPoints[N](labelKeys, ts)
	return metricdata.Gauge[N]{DataPoints: points}, err
}

// convertSum converts an OpenCensus cumulative to an OpenTelemetry sum aggregation.
func convertSum[N int64 | float64](labelKeys []ocmetricdata.LabelKey, ts []*ocmetricdata.TimeSeries) (metricdata.Sum[N], error) {
	points, err := convertNumberDataPoints[N](labelKeys, ts)
	// OpenCensus sums are always Cumulative
	return metricdata.Sum[N]{DataPoints: points, Temporality: metricdata.CumulativeTemporality, IsMonotonic: true}, err
}

// convertNumberDataPoints converts OpenCensus TimeSeries to OpenTelemetry DataPoints.
func convertNumberDataPoints[N int64 | float64](labelKeys []ocmetricdata.LabelKey, ts []*ocmetricdata.TimeSeries) ([]metricdata.DataPoint[N], error) {
	var points []metricdata.DataPoint[N]
	var err error
	for _, t := range ts {
		attrs, attrsErr := convertAttrs(labelKeys, t.LabelValues)
		if attrsErr != nil {
			err = errors.Join(err, attrsErr)
			continue
		}
		for _, p := range t.Points {
			v, ok := p.Value.(N)
			if !ok {
				err = errors.Join(err, fmt.Errorf("%w: %q", errMismatchedValueTypes, p.Value))
				continue
			}
			points = append(points, metricdata.DataPoint[N]{
				Attributes: attrs,
				StartTime:  t.StartTime,
				Time:       p.Time,
				Value:      v,
			})
		}
	}
	return points, err
}

// convertHistogram converts OpenCensus Distribution timeseries to an
// OpenTelemetry Histogram aggregation.
func convertHistogram(labelKeys []ocmetricdata.LabelKey, ts []*ocmetricdata.TimeSeries) (metricdata.Histogram[float64], error) {
	points := make([]metricdata.HistogramDataPoint[float64], 0, len(ts))
	var err error
	for _, t := range ts {
		attrs, attrsErr := convertAttrs(labelKeys, t.LabelValues)
		if attrsErr != nil {
			err = errors.Join(err, attrsErr)
			continue
		}
		for _, p := range t.Points {
			dist, ok := p.Value.(*ocmetricdata.Distribution)
			if !ok {
				err = errors.Join(err, fmt.Errorf("%w: %d", errMismatchedValueTypes, p.Value))
				continue
			}
			bucketCounts, bucketErr := convertBucketCounts(dist.Buckets)
			if bucketErr != nil {
				err = errors.Join(err, bucketErr)
				continue
			}
			if dist.Count < 0 {
				err = errors.Join(err, fmt.Errorf("%w: %d", errNegativeDistributionCount, dist.Count))
				continue
			}
			// TODO: handle exemplars
			points = append(points, metricdata.HistogramDataPoint[float64]{
				Attributes:   attrs,
				StartTime:    t.StartTime,
				Time:         p.Time,
				Count:        uint64(dist.Count),
				Sum:          dist.Sum,
				Bounds:       dist.BucketOptions.Bounds,
				BucketCounts: bucketCounts,
			})
		}
	}
	return metricdata.Histogram[float64]{DataPoints: points, Temporality: metricdata.CumulativeTemporality}, err
}

// convertBucketCounts converts from OpenCensus bucket counts to slice of uint64.
func convertBucketCounts(buckets []ocmetricdata.Bucket) ([]uint64, error) {
	bucketCounts := make([]uint64, len(buckets))
	for i, bucket := range buckets {
		if bucket.Count < 0 {
			return nil, fmt.Errorf("%w: %q", errNegativeBucketCount, bucket.Count)
		}
		bucketCounts[i] = uint64(bucket.Count)
	}
	return bucketCounts, nil
}

// convertAttrs converts from OpenCensus attribute keys and values to an
// OpenTelemetry attribute Set.
func convertAttrs(keys []ocmetricdata.LabelKey, values []ocmetricdata.LabelValue) (attribute.Set, error) {
	if len(keys) != len(values) {
		return attribute.NewSet(), fmt.Errorf("%w: keys(%q) values(%q)", errMismatchedAttributeKeyValues, len(keys), len(values))
	}
	attrs := []attribute.KeyValue{}
	for i, lv := range values {
		if !lv.Present {
			continue
		}
		attrs = append(attrs, attribute.KeyValue{
			Key:   attribute.Key(keys[i].Key),
			Value: attribute.StringValue(lv.Value),
		})
	}
	return attribute.NewSet(attrs...), nil
}
