// Copyright 2018 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package props

import (
	"bytes"
	"fmt"
	"math"
	"sort"

	"github.com/cockroachdb/cockroach/pkg/sql/opt"
)

// Statistics is a collection of measurements and statistics that is used by
// the coster to estimate the cost of expressions. Statistics are collected
// for tables and indexes and are exposed to the optimizer via opt.Catalog
// interfaces.
//
// As logical properties are derived bottom-up for each expression, the
// estimated row count is derived bottom-up for each relational expression.
// The column statistics (stored in ColStats and MultiColStats) are derived
// lazily, and only as needed to determine the row count for the current
// expression or a parent expression. For example:
//
//   SELECT y FROM a WHERE x=1
//
// The only column that affects the row count of this query is x, since the
// distribution of values in x is what determines the selectivity of the
// predicate. As a result, column statistics will be derived for column x but
// not for column y.
//
// See memo/statistics_builder.go for more information about how statistics are
// calculated.
type Statistics struct {
	// RowCount is the estimated number of rows returned by the expression.
	// Note that - especially when there are no stats available - the scaling of
	// the row counts can be unpredictable; thus, a row count of 0.001 should be
	// considered 1000 times better than a row count of 1, even though if this was
	// a true row count they would be pretty much the same thing.
	rowCount float64

	// ColStats is a collection of statistics that pertain to columns in an
	// expression or table. It is keyed by a set of one or more columns over which
	// the statistic is defined.
	ColStats ColStatsMap

	// Selectivity is a value between 0 and 1 representing the estimated
	// reduction in number of rows for the top-level operator in this
	// expression.
	selectivity float64
}

// Copy returns a copy of the given Statistics object
func (s *Statistics) Copy() Statistics {
	c := Statistics{}
	c.rowCount = s.rowCount
	c.ColStats = s.ColStats.Copy()
	c.selectivity = s.selectivity
	return c
}

// RowCount is a getter method for Statistics.rowCount
func (s *Statistics) RowCount() float64 {
	return s.rowCount
}

// UpdateRowCount is a setter method for Statistics.rowCount
func (s *Statistics) UpdateRowCount(rowCount float64) {
	s.rowCount = rowCount
}

// Selectivity is a getter method for Statistics.selectivity
func (s *Statistics) Selectivity() float64 {
	return s.selectivity
}

// UpdateSelectivity is a setter method for Statistics.selectivity
func (s *Statistics) UpdateSelectivity(selectivity float64) {
	s.selectivity = selectivity
}

// Init initializes the data members of Statistics.
func (s *Statistics) Init(relProps *Relational) (zeroCardinality bool) {
	if relProps.Cardinality.IsZero() {
		s.UpdateRowCount(0)
		s.UpdateSelectivity(0)
		return true
	}
	s.UpdateSelectivity(1)
	return false
}

// ApplySelectivity applies a given selectivity to the statistics. RowCount and
// Selectivity are updated. Note that DistinctCounts are not updated, other than
// limiting them to the new RowCount. See ColumnStatistic.ApplySelectivity for
// updating distinct counts.
func (s *Statistics) ApplySelectivity(selectivity float64) {
	if selectivity == 0 {
		s.UpdateRowCount(0)
		for i, n := 0, s.ColStats.Count(); i < n; i++ {
			s.ColStats.Get(i).UpdateDistinctCount(0)
		}
		return
	}

	s.UpdateRowCount(s.RowCount() * selectivity)
	s.UpdateSelectivity(s.Selectivity() * selectivity)

	// Make sure none of the distinct counts are larger than the row count.
	for i, n := 0, s.ColStats.Count(); i < n; i++ {
		colStat := s.ColStats.Get(i)
		if colStat.DistinctCount() > s.RowCount() {
			colStat.UpdateDistinctCount(s.RowCount())
		}
	}
}

func (s *Statistics) String() string {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "[rows=%.9g", s.RowCount())
	colStats := make(ColumnStatistics, s.ColStats.Count())
	for i := 0; i < s.ColStats.Count(); i++ {
		colStats[i] = s.ColStats.Get(i)
	}
	sort.Sort(colStats)
	for _, col := range colStats {
		fmt.Fprintf(&buf, ", distinct%s=%.9g", col.Cols.String(), col.DistinctCount())
	}
	buf.WriteString("]")

	return buf.String()
}

// ColumnStatistic is a collection of statistics that applies to a particular
// set of columns. In theory, a table could have a ColumnStatistic object
// for every possible subset of columns. In practice, it is only worth
// maintaining statistics on a few columns and column sets that are frequently
// used in predicates, group by columns, etc.
type ColumnStatistic struct {
	// Cols is the set of columns whose data are summarized by this
	// ColumnStatistic struct.
	Cols opt.ColSet

	// DistinctCount is the estimated number of distinct values of this
	// set of columns for this expression.
	distinctCount float64
}

// Copy returns a copy of the given ColumnStatistic
func (c *ColumnStatistic) Copy() ColumnStatistic {
	cp := ColumnStatistic{}
	cp.Cols = c.Cols.Copy()
	cp.distinctCount = c.distinctCount
	return cp
}

// UpdateDistinctCount is a setter method for ColumnStatistics.distinctCount
func (c *ColumnStatistic) UpdateDistinctCount(distinctCount float64) {
	c.distinctCount = distinctCount
}

// DistinctCount is a getter method for ColumnStatistics.distinctCount
func (c *ColumnStatistic) DistinctCount() float64 {
	return c.distinctCount
}

// ApplySelectivity updates the distinct count according to a given selectivity.
func (c *ColumnStatistic) ApplySelectivity(selectivity, inputRows float64) {
	if selectivity == 1 || c.DistinctCount() == 0 {
		return
	}
	if selectivity == 0 {
		c.UpdateDistinctCount(0)
		return
	}

	n := inputRows
	d := c.DistinctCount()

	// If each distinct value appears n/d times, and the probability of a
	// row being filtered out is (1 - selectivity), the probability that all
	// n/d rows are filtered out is (1 - selectivity)^(n/d). So the expected
	// number of values that are filtered out is d*(1 - selectivity)^(n/d).
	//
	// This formula returns d * selectivity when d=n but is closer to d
	// when d << n.
	c.UpdateDistinctCount(d - d*math.Pow(1-selectivity, n/d))
}

// ColumnStatistics is a slice of pointers to ColumnStatistic values.
type ColumnStatistics []*ColumnStatistic

// Len returns the number of ColumnStatistic values.
func (c ColumnStatistics) Len() int { return len(c) }

// Less is part of the Sorter interface.
func (c ColumnStatistics) Less(i, j int) bool {
	if c[i].Cols.Len() != c[j].Cols.Len() {
		return c[i].Cols.Len() < c[j].Cols.Len()
	}

	prev := 0
	for {
		nextI, ok := c[i].Cols.Next(prev)
		if !ok {
			return false
		}

		// No need to check if ok since both ColSets are the same length and
		// so far have had the same elements.
		nextJ, _ := c[j].Cols.Next(prev)

		if nextI != nextJ {
			return nextI < nextJ
		}

		prev = nextI
	}
}

// Swap is part of the Sorter interface.
func (c ColumnStatistics) Swap(i, j int) {
	c[i], c[j] = c[j], c[i]
}
