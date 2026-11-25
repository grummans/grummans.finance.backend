package budget

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func at(day, hour int) time.Time {
	return time.Date(2026, 11, day, hour, 0, 0, 0, ICT)
}

func TestCycleFor(t *testing.T) {
	tests := []struct {
		name      string
		now       time.Time
		receipts  []string
		wantStart string
		wantEnd   string
		wantDays  int
		projected bool
	}{
		{
			name:      "no receipt before the 10th projects this month's 10th",
			now:       at(6, 9),
			wantStart: "2026-11-06",
			wantEnd:   "2026-11-10",
			wantDays:  4,
			projected: true,
		},
		{
			name:      "on the 10th without a receipt the cycle runs to next month",
			now:       at(10, 9),
			wantStart: "2026-11-10",
			wantEnd:   "2026-12-10",
			wantDays:  30,
			projected: true,
		},
		{
			name:      "after the 10th the projection moves to next month",
			now:       at(11, 9),
			wantStart: "2026-11-11",
			wantEnd:   "2026-12-10",
			wantDays:  29,
			projected: true,
		},
		{
			name:      "a receipt in the current payday window still projects next month",
			now:       at(6, 9),
			receipts:  []string{"2026-11-05"},
			wantStart: "2026-11-05",
			wantEnd:   "2026-12-10",
			wantDays:  34,
			projected: true,
		},
		{
			name:      "a late receipt leaves the current 10th as the cycle end",
			now:       at(1, 9),
			receipts:  []string{"2026-10-30"},
			wantStart: "2026-10-30",
			wantEnd:   "2026-11-10",
			wantDays:  9,
			projected: true,
		},
		{
			name:      "a projection already in the past moves to the next 10th",
			now:       at(12, 9),
			receipts:  []string{"2026-10-30"},
			wantStart: "2026-10-30",
			wantEnd:   "2026-12-10",
			wantDays:  28,
			projected: true,
		},
		{
			name:      "the latest receipt starts the cycle",
			now:       time.Date(2026, 11, 20, 9, 0, 0, 0, ICT),
			receipts:  []string{"2026-10-05", "2026-11-05"},
			wantStart: "2026-11-05",
			wantEnd:   "2026-12-10",
			wantDays:  20,
			projected: true,
		},
		{
			name:      "a receipt recorded today starts a full month",
			now:       time.Date(2026, 11, 25, 9, 0, 0, 0, ICT),
			receipts:  []string{"2026-11-25"},
			wantStart: "2026-11-25",
			wantEnd:   "2026-12-10",
			wantDays:  15,
			projected: true,
		},
		{
			name:      "february in a common year",
			now:       time.Date(2027, 2, 20, 9, 0, 0, 0, ICT),
			receipts:  []string{"2027-01-30"},
			wantStart: "2027-01-30",
			wantEnd:   "2027-03-10",
			wantDays:  18,
			projected: true,
		},
		{
			name:      "february in a leap year",
			now:       time.Date(2028, 2, 20, 9, 0, 0, 0, ICT),
			receipts:  []string{"2028-01-30"},
			wantStart: "2028-01-30",
			wantEnd:   "2028-03-10",
			wantDays:  19,
			projected: true,
		},
		{
			name:      "december rolls the projection into the next year",
			now:       time.Date(2026, 12, 12, 9, 0, 0, 0, ICT),
			wantStart: "2026-12-12",
			wantEnd:   "2027-01-10",
			wantDays:  29,
			projected: true,
		},
		{
			name:      "a recorded future payday ends the projection",
			now:       at(6, 9),
			receipts:  []string{"2026-11-20"},
			wantStart: "2026-11-06",
			wantEnd:   "2026-11-20",
			wantDays:  14,
			projected: false,
		},
		{
			name:      "23:30 in Ho Chi Minh City is still the same business day",
			now:       time.Date(2026, 11, 9, 16, 30, 0, 0, time.UTC),
			wantStart: "2026-11-09",
			wantEnd:   "2026-11-10",
			wantDays:  1,
			projected: true,
		},
		{
			name:      "00:30 in Ho Chi Minh City is the next business day",
			now:       time.Date(2026, 11, 9, 17, 30, 0, 0, time.UTC),
			wantStart: "2026-11-10",
			wantEnd:   "2026-12-10",
			wantDays:  30,
			projected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			receipts := make([]Receipt, 0, len(tt.receipts))
			for _, day := range tt.receipts {
				receipts = append(receipts, Receipt{ReceivedOn: day})
			}
			cycle := CycleFor(tt.now, receipts)
			if cycle.Start != tt.wantStart || cycle.End != tt.wantEnd {
				t.Fatalf("cycle = %s..%s, want %s..%s", cycle.Start, cycle.End, tt.wantStart, tt.wantEnd)
			}
			if cycle.DaysRemaining != tt.wantDays {
				t.Fatalf("days remaining = %d, want %d", cycle.DaysRemaining, tt.wantDays)
			}
			if cycle.Projected != tt.projected {
				t.Fatalf("projected = %v, want %v", cycle.Projected, tt.projected)
			}
		})
	}
}

func TestBuild(t *testing.T) {
	cycle := Cycle{Start: "2026-11-05", End: "2026-12-10", DaysRemaining: 34}
	projectedCycle := cycle
	projectedCycle.Projected = true
	today := at(6, 9)
	budget := func(amount int64) *int64 { return &amount }

	tests := []struct {
		name          string
		cycle         Cycle
		settings      Settings
		data          Data
		wantConfirmed int64
		wantExpected  int64
		wantAvailable int64
		wantSpendable int64
		wantDaily     int64
		wantWarnings  string
		wantCertainty []string
		wantOverflow  bool
	}{
		{
			name:     "reserves, overdue and estimated bills",
			cycle:    cycle,
			settings: Settings{GlobalBufferVND: 1_000_000},
			data: Data{
				CashOnHandVND: 10_000_000,
				Occurrences: []Occurrence{
					{ID: "rent", ObligationName: "Nha", DueDate: "2026-11-30", RemainingVND: 3_700_000},
					{ID: "bill", ObligationName: "Dien", DueDate: "2026-11-05", RemainingVND: 500_000, IsEstimate: true},
					{ID: "paid", ObligationName: "Internet", DueDate: "2026-11-07", RemainingVND: 0},
				},
			},
			wantConfirmed: 3_700_000,
			wantExpected:  500_000,
			wantAvailable: 4_800_000,
			wantSpendable: 4_800_000,
			wantDaily:     141_176,
			wantWarnings:  "OVERDUE,ESTIMATED_RESERVE",
			wantCertainty: []string{"confirmed", "expected"},
		},
		{
			name:     "leftover cents are dropped, not rounded up",
			cycle:    cycle,
			settings: Settings{GlobalBufferVND: 1_000_000},
			data:     Data{CashOnHandVND: 5_200_000},
			// 4.200.000 / 34 = 123.529.4, so the day keeps only whole dong
			wantAvailable: 4_200_000,
			wantSpendable: 4_200_000,
			wantDaily:     123_529,
			wantWarnings:  "",
		},
		{
			name:     "reserves above cash are a shortfall, never a negative day",
			cycle:    projectedCycle,
			settings: Settings{},
			data: Data{
				CashOnHandVND: 1_000_000,
				Occurrences: []Occurrence{
					{ID: "rent", ObligationName: "Nha", DueDate: "2026-11-30", RemainingVND: 3_700_000},
				},
			},
			wantConfirmed: 3_700_000,
			wantAvailable: -2_700_000,
			wantSpendable: 0,
			wantDaily:     0,
			wantWarnings:  "SHORTFALL,PROJECTED_PAYDAY",
			wantCertainty: []string{"confirmed"},
		},
		{
			name:     "a zero balance after the buffer is not a shortfall",
			cycle:    cycle,
			settings: Settings{GlobalBufferVND: 1_000_000},
			data:     Data{CashOnHandVND: 1_000_000},
			// exactly zero available is the boundary between allowed and short
			wantAvailable: 0,
			wantSpendable: 0,
			wantDaily:     0,
			wantWarnings:  "",
		},
		{
			name:     "the total budget caps what is spendable",
			cycle:    cycle,
			settings: Settings{GlobalBudgetVND: budget(3_000_000)},
			data:     Data{CashOnHandVND: 10_000_000, SpentInCycleVND: 2_000_000},
			// min(10.000.000, 3.000.000 - 2.000.000) = 1.000.000
			wantAvailable: 10_000_000,
			wantSpendable: 1_000_000,
			wantDaily:     29_411,
			wantWarnings:  "",
		},
		{
			name:          "spending above the total budget leaves nothing",
			cycle:         cycle,
			settings:      Settings{GlobalBudgetVND: budget(1_000_000)},
			data:          Data{CashOnHandVND: 10_000_000, SpentInCycleVND: 1_500_000},
			wantAvailable: 10_000_000,
			wantSpendable: 0,
			wantDaily:     0,
			wantWarnings:  "BUDGET_EXCEEDED",
		},
		{
			name:     "reserves that overflow int64 are an error",
			cycle:    cycle,
			settings: Settings{GlobalBufferVND: 1_000_000},
			data: Data{
				CashOnHandVND: 10_000_000,
				Occurrences: []Occurrence{
					{ID: "a", ObligationName: "Nha", DueDate: "2026-11-30", RemainingVND: math.MaxInt64},
					{ID: "b", ObligationName: "Dien", DueDate: "2026-11-05", RemainingVND: math.MaxInt64},
				},
			},
			wantOverflow: true,
		},
		{
			name:          "the smallest available amount is still a shortfall, not an overflow",
			cycle:         cycle,
			settings:      Settings{},
			data:          Data{Occurrences: []Occurrence{{ID: "a", ObligationName: "Nha", DueDate: "2026-11-30", RemainingVND: math.MaxInt64}}},
			wantConfirmed: math.MaxInt64,
			wantAvailable: -math.MaxInt64,
			wantCertainty: []string{"confirmed"},
			wantDaily:     0,
			wantWarnings:  "SHORTFALL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot, err := Build(tt.cycle, tt.settings, tt.data, today)
			if tt.wantOverflow {
				if !errors.Is(err, ErrAmountOverflow) {
					t.Fatalf("build error = %v, want ErrAmountOverflow", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("building snapshot: %v", err)
			}
			if snapshot.ReservedConfirmedVND != tt.wantConfirmed {
				t.Fatalf("confirmed reserve = %d, want %d", snapshot.ReservedConfirmedVND, tt.wantConfirmed)
			}
			if snapshot.ReservedExpectedVND != tt.wantExpected {
				t.Fatalf("expected reserve = %d, want %d", snapshot.ReservedExpectedVND, tt.wantExpected)
			}
			if snapshot.AvailableVND != tt.wantAvailable {
				t.Fatalf("available = %d, want %d", snapshot.AvailableVND, tt.wantAvailable)
			}
			if snapshot.SpendableVND != tt.wantSpendable {
				t.Fatalf("spendable = %d, want %d", snapshot.SpendableVND, tt.wantSpendable)
			}
			if snapshot.DailyAllowanceVND != tt.wantDaily {
				t.Fatalf("daily allowance = %d, want %d", snapshot.DailyAllowanceVND, tt.wantDaily)
			}
			if got := strings.Join(snapshot.Warnings, ","); got != tt.wantWarnings {
				t.Fatalf("warnings = %q, want %q", got, tt.wantWarnings)
			}
			if len(snapshot.Forecast) != len(tt.wantCertainty) {
				t.Fatalf("forecast has %d items, want %d", len(snapshot.Forecast), len(tt.wantCertainty))
			}
			for index, certainty := range tt.wantCertainty {
				if snapshot.Forecast[index].Certainty != certainty {
					t.Fatalf("forecast[%d] certainity = %s, want %s", index, snapshot.Forecast[index].Certainty, certainty)
				}
			}
		})
	}
}

func TestBuildMarksOnlyPastDueItemsOverdue(t *testing.T) {
	cycle := Cycle{Start: "2026-11-05", End: "2026-12-10", DaysRemaining: 34}
	today := at(6, 9)
	snapshot, err := Build(cycle, Settings{}, Data{
		CashOnHandVND: 10_000_000,
		Occurrences: []Occurrence{
			{ID: "today", DueDate: "2026-11-06", RemainingVND: 100},
			{ID: "yesterday", DueDate: "2026-11-05", RemainingVND: 100},
		},
	}, today)
	if err != nil {
		t.Fatalf("building snapshot: %v", err)
	}

	if snapshot.Forecast[0].Overdue {
		t.Fatal("an obligation due today is not overdue until the next day (D-029)")
	}
	if !snapshot.Forecast[1].Overdue {
		t.Fatal("an obligation due yesterday is overdue")
	}
	if got := strings.Join(snapshot.Warnings, ","); got != "OVERDUE" {
		t.Fatalf("warnings = %q, want OVERDUE", got)
	}
}
