package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"google.golang.org/protobuf/proto"
)

// fromProto converts the families gathered by the Prometheus registry to
// the neutral form.
func fromProto(mfs []*dto.MetricFamily) []Family {
	out := make([]Family, 0, len(mfs))
	for _, mf := range mfs {
		f := Family{Name: mf.GetName(), Help: mf.GetHelp(), Kind: kindOf(mf.GetType())}
		f.Samples = make([]Sample, 0, len(mf.GetMetric()))
		for _, m := range mf.GetMetric() {
			s := Sample{Labels: make(map[string]string, len(m.GetLabel()))}
			for _, lp := range m.GetLabel() {
				s.Labels[lp.GetName()] = lp.GetValue()
			}
			switch f.Kind {
			case KindCounter:
				s.Value = m.GetCounter().GetValue()
			case KindGauge:
				s.Value = m.GetGauge().GetValue()
			case KindHistogram:
				h := m.GetHistogram()
				s.Count, s.Sum = h.GetSampleCount(), h.GetSampleSum()
				for _, b := range h.GetBucket() {
					s.Buckets = append(s.Buckets, Bucket{UpperBound: b.GetUpperBound(), Count: b.GetCumulativeCount()})
				}
			case KindSummary:
				sm := m.GetSummary()
				s.Count, s.Sum = sm.GetSampleCount(), sm.GetSampleSum()
				for _, q := range sm.GetQuantile() {
					s.Quantiles = append(s.Quantiles, Quantile{Quantile: q.GetQuantile(), Value: q.GetValue()})
				}
			default:
				s.Value = m.GetUntyped().GetValue()
			}
			f.Samples = append(f.Samples, s)
		}
		out = append(out, f)
	}
	return out
}

func kindOf(t dto.MetricType) Kind {
	switch t {
	case dto.MetricType_COUNTER:
		return KindCounter
	case dto.MetricType_GAUGE:
		return KindGauge
	case dto.MetricType_HISTOGRAM:
		return KindHistogram
	case dto.MetricType_SUMMARY:
		return KindSummary
	default:
		return KindUntyped
	}
}

// toProto converts a family back to the Prometheus form, for the text
// encoder: the labels of every sample are sorted by name.
func toProto(f Family) *dto.MetricFamily {
	mf := &dto.MetricFamily{Name: proto.String(f.Name), Help: proto.String(f.Help)}
	switch f.Kind {
	case KindCounter:
		mf.Type = dto.MetricType_COUNTER.Enum()
	case KindGauge:
		mf.Type = dto.MetricType_GAUGE.Enum()
	case KindHistogram:
		mf.Type = dto.MetricType_HISTOGRAM.Enum()
	case KindSummary:
		mf.Type = dto.MetricType_SUMMARY.Enum()
	default:
		mf.Type = dto.MetricType_UNTYPED.Enum()
	}
	for _, s := range f.Samples {
		m := &dto.Metric{}
		names := make([]string, 0, len(s.Labels))
		for k := range s.Labels {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			m.Label = append(m.Label, &dto.LabelPair{Name: proto.String(k), Value: proto.String(s.Labels[k])})
		}
		switch f.Kind {
		case KindCounter:
			m.Counter = &dto.Counter{Value: proto.Float64(s.Value)}
		case KindGauge:
			m.Gauge = &dto.Gauge{Value: proto.Float64(s.Value)}
		case KindHistogram:
			h := &dto.Histogram{SampleCount: proto.Uint64(s.Count), SampleSum: proto.Float64(s.Sum)}
			for _, b := range s.Buckets {
				h.Bucket = append(h.Bucket, &dto.Bucket{UpperBound: proto.Float64(b.UpperBound), CumulativeCount: proto.Uint64(b.Count)})
			}
			m.Histogram = h
		case KindSummary:
			sm := &dto.Summary{SampleCount: proto.Uint64(s.Count), SampleSum: proto.Float64(s.Sum)}
			for _, q := range s.Quantiles {
				sm.Quantile = append(sm.Quantile, &dto.Quantile{Quantile: proto.Float64(q.Quantile), Value: proto.Float64(q.Value)})
			}
			m.Summary = sm
		default:
			m.Untyped = &dto.Untyped{Value: proto.Float64(s.Value)}
		}
		mf.Metric = append(mf.Metric, m)
	}
	return mf
}

// writeText encodes families in the text exposition format, sorted by
// name; a family without samples is skipped (the format has no use for
// it).
func writeText(w io.Writer, families []Family) error {
	sorted := append([]Family(nil), families...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, f := range sorted {
		if len(f.Samples) == 0 {
			continue
		}
		if _, err := expfmt.MetricFamilyToText(w, toProto(f)); err != nil {
			return fmt.Errorf("metrics: encode %s: %w", f.Name, err)
		}
	}
	return nil
}

// sampleKey identifies a sample of a family by its sorted labels.
func sampleKey(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for k := range labels {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte('\xff')
	}
	return b.String()
}

// withLabel returns a copy of the families with the label name=value
// added to every sample (an existing label of the same name is kept).
func withLabel(families []Family, name, value string) []Family {
	out := make([]Family, len(families))
	for i, f := range families {
		out[i] = f
		out[i].Samples = make([]Sample, len(f.Samples))
		for j, s := range f.Samples {
			labels := make(map[string]string, len(s.Labels)+1)
			for k, v := range s.Labels {
				labels[k] = v
			}
			if _, ok := labels[name]; !ok {
				labels[name] = value
			}
			s.Labels = labels
			out[i].Samples[j] = s
		}
	}
	return out
}

// mergeFamilies merges the families of several sources by name: the
// samples are concatenated, those with the same labels are summed when sum
// is set (the bucket scope) and kept once otherwise. A family whose kind
// differs from the first one met is dropped.
func mergeFamilies(sum bool, sources ...[]Family) []Family {
	index := map[string]int{}
	var out []Family
	samples := []map[string]int{}
	for _, src := range sources {
		for _, f := range src {
			i, ok := index[f.Name]
			if !ok {
				i = len(out)
				index[f.Name] = i
				out = append(out, Family{Name: f.Name, Help: f.Help, Kind: f.Kind})
				samples = append(samples, map[string]int{})
			}
			if out[i].Kind != f.Kind {
				continue
			}
			for _, s := range f.Samples {
				key := sampleKey(s.Labels)
				j, seen := samples[i][key]
				if !seen {
					samples[i][key] = len(out[i].Samples)
					out[i].Samples = append(out[i].Samples, cloneSample(s))
					continue
				}
				if sum {
					addSample(&out[i].Samples[j], s)
				}
			}
		}
	}
	return out
}

// cloneSample copies the slices of a sample (the merges add into them).
func cloneSample(s Sample) Sample {
	s.Buckets = append([]Bucket(nil), s.Buckets...)
	s.Quantiles = append([]Quantile(nil), s.Quantiles...)
	return s
}

// addSample adds s into dst: values, counts, sums and the buckets of the
// same bounds (a summary keeps the quantiles of dst: they do not add).
func addSample(dst *Sample, s Sample) {
	dst.Value += s.Value
	dst.Count += s.Count
	dst.Sum += s.Sum
	for _, b := range s.Buckets {
		for k := range dst.Buckets {
			if dst.Buckets[k].UpperBound == b.UpperBound || (math.IsInf(b.UpperBound, 1) && math.IsInf(dst.Buckets[k].UpperBound, 1)) {
				dst.Buckets[k].Count += b.Count
				break
			}
		}
	}
}
