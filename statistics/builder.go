// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package statistics

import (
	"github.com/juju/errors"
	"github.com/pingcap/tidb/ast"
	"github.com/pingcap/tidb/context"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tidb/util/types"
)

// Builder describes information needed when build index or column.
type Builder struct {
	Ctx        context.Context  // Ctx is the context.
	TblInfo    *model.TableInfo // TblInfo is the table info of the table.
	NumBuckets int64            // NumBuckets is the number of buckets a column histogram has.
}

// BuildIndex builds histogram for index or pk.
func (b *Builder) BuildIndex(id int64, records ast.RecordSet, isIndex int) (int64, *Histogram, error) {
	hg := &Histogram{
		ID:      id,
		NDV:     0,
		Buckets: make([]bucket, 1, b.NumBuckets),
	}
	var valuesPerBucket, lastNumber, bucketIdx int64 = 1, 0, 0
	count := int64(0)
	sc := b.Ctx.GetSessionVars().StmtCtx
	for {
		row, err := records.Next()
		if err != nil {
			return 0, nil, errors.Trace(err)
		}
		if row == nil {
			break
		}
		var data types.Datum
		if isIndex == 0 {
			data = row.Data[0]
		} else {
			bytes, err := codec.EncodeKey(nil, row.Data...)
			if err != nil {
				return 0, nil, errors.Trace(err)
			}
			data = types.NewBytesDatum(bytes)
		}
		cmp, err := hg.Buckets[bucketIdx].Value.CompareDatum(sc, data)
		if err != nil {
			return 0, nil, errors.Trace(err)
		}
		count++
		if cmp == 0 {
			// The new item has the same value as current bucket value, to ensure that
			// a same value only stored in a single bucket, we do not increase bucketIdx even if it exceeds
			// valuesPerBucket.
			hg.Buckets[bucketIdx].Count++
			hg.Buckets[bucketIdx].Repeats++
		} else if hg.Buckets[bucketIdx].Count+1-lastNumber <= valuesPerBucket {
			// The bucket still have room to store a new item, update the bucket.
			hg.Buckets[bucketIdx].Count++
			hg.Buckets[bucketIdx].Value = data
			hg.Buckets[bucketIdx].Repeats = 1
			hg.NDV++
		} else {
			// All buckets are full, we should merge buckets.
			if bucketIdx+1 == b.NumBuckets {
				hg.mergeBuckets(bucketIdx)
				valuesPerBucket *= 2
				bucketIdx = bucketIdx / 2
				if bucketIdx == 0 {
					lastNumber = 0
				} else {
					lastNumber = hg.Buckets[bucketIdx-1].Count
				}
			}
			// We may merge buckets, so we should check it again.
			if hg.Buckets[bucketIdx].Count+1-lastNumber <= valuesPerBucket {
				hg.Buckets[bucketIdx].Count++
				hg.Buckets[bucketIdx].Value = data
				hg.Buckets[bucketIdx].Repeats = 1
			} else {
				lastNumber = hg.Buckets[bucketIdx].Count
				bucketIdx++
				hg.Buckets = append(hg.Buckets, bucket{
					Count:   lastNumber + 1,
					Value:   data,
					Repeats: 1,
				})
			}
			hg.NDV++
		}
	}
	if count == 0 {
		hg = &Histogram{ID: id}
	}
	return count, hg, nil
}

// BuildColumn builds histogram from samples for column.
func (b *Builder) BuildColumn(id int64, ndv int64, count int64, samples []types.Datum) (*Histogram, error) {
	if count == 0 {
		return &Histogram{ID: id}, nil
	}
	sc := b.Ctx.GetSessionVars().StmtCtx
	err := types.SortDatums(sc, samples)
	if err != nil {
		return nil, errors.Trace(err)
	}
	hg := &Histogram{
		ID:      id,
		NDV:     ndv,
		Buckets: make([]bucket, 1, b.NumBuckets),
	}
	valuesPerBucket := float64(count)/float64(b.NumBuckets) + 1

	// As we use samples to build the histogram, the bucket number and repeat should multiply a factor.
	sampleFactor := float64(count) / float64(len(samples))
	ndvFactor := float64(count) / float64(ndv)
	if ndvFactor > sampleFactor {
		ndvFactor = sampleFactor
	}
	bucketIdx := 0
	var lastCount int64
	for i := int64(0); i < int64(len(samples)); i++ {
		cmp, err := hg.Buckets[bucketIdx].Value.CompareDatum(sc, samples[i])
		if err != nil {
			return nil, errors.Trace(err)
		}
		totalCount := float64(i+1) * sampleFactor
		if cmp == 0 {
			// The new item has the same value as current bucket value, to ensure that
			// a same value only stored in a single bucket, we do not increase bucketIdx even if it exceeds
			// valuesPerBucket.
			hg.Buckets[bucketIdx].Count = int64(totalCount)
			if float64(hg.Buckets[bucketIdx].Repeats) == ndvFactor {
				hg.Buckets[bucketIdx].Repeats = int64(2 * sampleFactor)
			} else {
				hg.Buckets[bucketIdx].Repeats += int64(sampleFactor)
			}
		} else if totalCount-float64(lastCount) <= valuesPerBucket {
			// The bucket still have room to store a new item, update the bucket.
			hg.Buckets[bucketIdx].Count = int64(totalCount)
			hg.Buckets[bucketIdx].Value = samples[i]
			hg.Buckets[bucketIdx].Repeats = int64(ndvFactor)
		} else {
			lastCount = hg.Buckets[bucketIdx].Count
			// The bucket is full, store the item in the next bucket.
			bucketIdx++
			hg.Buckets = append(hg.Buckets, bucket{
				Count:   int64(totalCount),
				Value:   samples[i],
				Repeats: int64(ndvFactor),
			})
		}
	}
	return hg, nil
}

// CopyFromIndexColumns is used to replace the sampled column histogram with index histogram if the
// index is single column index.
// Index histogram is encoded, it need to be decoded to be used as column histogram.
// TODO: use field type to decode the value.
func CopyFromIndexColumns(ind *Index, id int64) (*Column, error) {
	hg := Histogram{
		ID:      id,
		NDV:     ind.NDV,
		Buckets: make([]bucket, 0, len(ind.Buckets)),
	}
	for _, b := range ind.Buckets {
		val := b.Value
		if val.GetBytes() == nil {
			break
		}
		data, err := codec.Decode(val.GetBytes(), 1)
		if err != nil {
			return nil, errors.Trace(err)
		}
		hg.Buckets = append(hg.Buckets, bucket{
			Count:   b.Count,
			Value:   data[0],
			Repeats: b.Repeats,
		})
	}
	return &Column{Histogram: hg}, nil
}
