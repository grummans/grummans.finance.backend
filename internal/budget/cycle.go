// Package budget computes the salary cycle, the reserved obligations and the
// daily spendable allowance. The money rules live in pure functions so the
// boundary cases are testable without a database.
package budget

import (
	"errors"
	"time"
)

// ErrAmountOverflow means the stored money no longer fits int64 VND, so no
// allowance can be computed from it.
var ErrAmountOverflow = errors.New("budget: money amounts overflow int64")

// ICT is the business timezone. Vietnam has no daylight saving since 1975, so a
// fixed zone keeps date maths independent of the host tz database.
var ICT = time.FixedZone("Asia/Ho_Chi_Minh", 7*60*60)

// projectedPaydayDay is the latest payday in the observed 5th-10th window. Until
// the next payday is known it is the conservative end of the cycle (D-033).
const projectedPaydayDay = 10

const dateLayout = "2006-01-02"

// Receipt is one actual salary receipt (D-010). ReceivedOn is a business date.
type Receipt struct {
	ID         string    `json:"id"`
	AmountVND  int64     `json:"amount_vnd"`
	ReceivedOn string    `json:"received_on"`
	ReceivedAt time.Time `json:"received_at"`
}

// Cycle is the half-open window [Start, End) between two paydays.
type Cycle struct {
	Start         string `json:"start"`
	End           string `json:"end"`
	DaysRemaining int    `json:"days_remaining"`
	// Projected is true when End is the day-10 projection, not a recorded payday.
	Projected bool `json:"projected"`
}

// Settings are the user-entered budget knobs; they are never hardcoded.
type Settings struct {
	GlobalBufferVND int64  `json:"global_buffer_vnd"`
	GlobalBudgetVND *int64 `json:"global_budget_vnd"`
}

// Occurrence is an unpaid obligation occurrence due before the cycle ends.
type Occurrence struct {
	ID             string
	ObligationName string
	DueDate        string
	RemainingVND   int64
	IsEstimate     bool
}

// Data is the stored state one snapshot is built from.
type Data struct {
	CashOnHandVND   int64
	SpentInCycleVND int64
	Occurrences     []Occurrence
}

// ForecastItem is one obligation still to pay, marked as booked or estimated.
type ForecastItem struct {
	OccurrenceID   string `json:"occurrence_id"`
	ObligationName string `json:"obligation_name"`
	DueDate        string `json:"due_date"`
	RemainingVND   int64  `json:"remaining_vnd"`
	Certainty      string `json:"certainty"`
	Overdue        bool   `json:"overdue"`
}

// Snapshot is the whole budget answer for one point in time.
type Snapshot struct {
	Cycle                Cycle          `json:"cycle"`
	CashOnHandVND        int64          `json:"cash_on_hand_vnd"`
	ReservedConfirmedVND int64          `json:"reserved_confirmed_vnd"`
	ReservedExpectedVND  int64          `json:"reserved_expected_vnd"`
	GlobalBufferVND      int64          `json:"global_buffer_vnd"`
	GlobalBudgetVND      *int64         `json:"global_budget_vnd"`
	SpentInCycleVND      int64          `json:"spent_in_cycle_vnd"`
	AvailableVND         int64          `json:"available_vnd"`
	SpendableVND         int64          `json:"spendable_vnd"`
	DailyAllowanceVND    int64          `json:"daily_allowance_vnd"`
	Warnings             []string       `json:"warnings"`
	Forecast             []ForecastItem `json:"forecast"`
}

// BusinessDate truncates an instant to its Asia/Ho_Chi_Minh calendar date.
func BusinessDate(now time.Time) time.Time {
	local := now.In(ICT)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, ICT)
}

// ParseBusinessDate reads a YYYY-MM-DD business date.
func ParseBusinessDate(value string) (time.Time, error) {
	return time.ParseInLocation(dateLayout, value, ICT)
}

// CycleFor derives the salary cycle from the recorded receipts (D-010). The next
// payday is the next recorded receipt after today; when none is recorded it is
// projected to the 10th of this month, or of the next month once the 10th has
// passed (D-033). On the 10th itself the projection moves to next month, which is
// the most conservative reading of a payday somewhere in the 5th-10th window.
func CycleFor(now time.Time, receipts []Receipt) Cycle {
	today := BusinessDate(now)
	start := today
	started := false
	var next time.Time
	for _, receipt := range receipts {
		day, err := ParseBusinessDate(receipt.ReceivedOn)
		if err != nil {
			continue
		}
		if !day.After(today) && (!started || day.After(start)) {
			start, started = day, true
		}
		if day.After(today) && (next.IsZero() || day.Before(next)) {
			next = day
		}
	}

	projected := next.IsZero()
	if projected {
		next = projectedPayday(start, started, today)
	}

	days := int(next.Sub(today).Hours() / 24)
	if days < 1 {
		days = 1 // a payday can never leave a zero-day cycle to divide by
	}
	return Cycle{
		Start:         start.Format(dateLayout),
		End:           next.Format(dateLayout),
		DaysRemaining: days,
		Projected:     projected,
	}
}

// projectedPayday is the payday assumed while the next one is not known: the 10th
// of the month after the last receipt, because a pay period is a month (D-010).
// Once that day is already past — a late or unrecorded salary — the next 10th
// after today is used instead, so the cycle never ends in the past. With no
// receipt at all, today's next 10th is the only anchor (D-033).
func projectedPayday(lastPayday time.Time, hasPayday bool, today time.Time) time.Time {
	if hasPayday {
		year, month, _ := lastPayday.Date()
		candidate := time.Date(year, month+1, projectedPaydayDay, 0, 0, 0, 0, ICT)
		if candidate.After(today) {
			return candidate
		}
	}
	year, month, _ := today.Date()
	if today.Day() >= projectedPaydayDay {
		month++
	}
	return time.Date(year, month, projectedPaydayDay, 0, 0, 0, 0, ICT)
}

// Build assembles one snapshot from the cycle, the settings and the stored state.
// available = cash - reserves (booked + estimated) - buffer (D-034); a total
// budget, when set, can only lower what is spendable. A negative available is
// reported as a zero allowance plus a SHORTFALL warning, never as a negative day.
// Amounts that no longer fit int64 are an error, never a wrapped allowance.
func Build(cycle Cycle, settings Settings, data Data, now time.Time) (Snapshot, error) {
	today := BusinessDate(now).Format(dateLayout)
	snapshot := Snapshot{
		Cycle:           cycle,
		CashOnHandVND:   data.CashOnHandVND,
		GlobalBufferVND: settings.GlobalBufferVND,
		GlobalBudgetVND: settings.GlobalBudgetVND,
		SpentInCycleVND: data.SpentInCycleVND,
		Forecast:        make([]ForecastItem, 0, len(data.Occurrences)),
		Warnings:        make([]string, 0, 4),
	}

	overdue := 0
	estimated := false
	for _, occurrence := range data.Occurrences {
		if occurrence.RemainingVND <= 0 {
			continue
		}
		item := ForecastItem{
			OccurrenceID:   occurrence.ID,
			ObligationName: occurrence.ObligationName,
			DueDate:        occurrence.DueDate,
			RemainingVND:   occurrence.RemainingVND,
			Certainty:      "confirmed",
			Overdue:        occurrence.DueDate < today,
		}
		if occurrence.IsEstimate {
			item.Certainty = "expected"
			estimated = true
			reserved, err := addVND(snapshot.ReservedExpectedVND, occurrence.RemainingVND)
			if err != nil {
				return Snapshot{}, err
			}
			snapshot.ReservedExpectedVND = reserved
		} else {
			reserved, err := addVND(snapshot.ReservedConfirmedVND, occurrence.RemainingVND)
			if err != nil {
				return Snapshot{}, err
			}
			snapshot.ReservedConfirmedVND = reserved
		}
		if item.Overdue {
			overdue++
		}
		snapshot.Forecast = append(snapshot.Forecast, item)
	}

	reserved, err := addVND(snapshot.ReservedConfirmedVND, snapshot.ReservedExpectedVND)
	if err != nil {
		return Snapshot{}, err
	}
	available, err := subVND(snapshot.CashOnHandVND, reserved)
	if err != nil {
		return Snapshot{}, err
	}
	available, err = subVND(available, snapshot.GlobalBufferVND)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot.AvailableVND = available

	spendable := snapshot.AvailableVND
	if spendable < 0 {
		spendable = 0
	}
	if settings.GlobalBudgetVND != nil {
		remainingBudget, err := subVND(*settings.GlobalBudgetVND, data.SpentInCycleVND)
		if err != nil {
			return Snapshot{}, err
		}
		if remainingBudget < 0 {
			remainingBudget = 0
		}
		if remainingBudget < spendable {
			spendable = remainingBudget
		}
	}
	snapshot.SpendableVND = spendable
	snapshot.DailyAllowanceVND = spendable / int64(cycle.DaysRemaining)

	if snapshot.AvailableVND < 0 {
		snapshot.Warnings = append(snapshot.Warnings, "SHORTFALL")
	}
	if overdue > 0 {
		snapshot.Warnings = append(snapshot.Warnings, "OVERDUE")
	}
	if estimated {
		snapshot.Warnings = append(snapshot.Warnings, "ESTIMATED_RESERVE")
	}
	if cycle.Projected {
		snapshot.Warnings = append(snapshot.Warnings, "PROJECTED_PAYDAY")
	}
	if settings.GlobalBudgetVND != nil && data.SpentInCycleVND > *settings.GlobalBudgetVND {
		snapshot.Warnings = append(snapshot.Warnings, "BUDGET_EXCEEDED")
	}
	return snapshot, nil
}

// addVND and subVND keep the money arithmetic inside int64; a wrapped total
// would otherwise turn a shortfall into an allowance.
func addVND(total, value int64) (int64, error) {
	sum := total + value
	if (value > 0 && sum < total) || (value < 0 && sum > total) {
		return 0, ErrAmountOverflow
	}
	return sum, nil
}

func subVND(minuend, subtrahend int64) (int64, error) {
	difference := minuend - subtrahend
	if (subtrahend > 0 && difference > minuend) || (subtrahend < 0 && difference < minuend) {
		return 0, ErrAmountOverflow
	}
	return difference, nil
}
