package sim

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// BookResult classifies a booking attempt's outcome for the agent that called
// it — mirrors Airline Sim's/Hotel Sim's three-way "ok"/"duplicate"/"sold out"
// result.
type BookResult string

const (
	ResultOK        BookResult = "ok"
	ResultDuplicate BookResult = "duplicate"
	ResultSoldOut   BookResult = "sold_out"
)

// sqlBooker is the ONE booking implementation used against every target
// family. Unlike Airline Sim's withRetry (needed because Galera's synchronous
// certification surfaces as a client-visible MySQL error 1213 that must be
// retried by the client itself), no PostgreSQL topology this app can be linked
// to has a client-visible synchronous write conflict: standalone/Patroni/
// repmgr are all single-writer at any moment (Patroni/repmgr just move which
// node that is, transparently to an already-open write connection failing over
// to a new one — an error, but not a retry-this-transaction error), and Spock's
// multi-master conflict resolution is asynchronous and invisible to the client
// entirely. So there is no retry loop anywhere in this file — every operation
// is a single transaction, once.
type sqlBooker struct {
	e *Engine
}

// withTx runs fn inside a single transaction — no retry wrapper (see the
// sqlBooker doc comment for why none is needed on any PostgreSQL topology this
// app targets).
func (b *sqlBooker) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := b.e.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func isDuplicateKey(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" // unique_violation
}

// errSoldOut / errDuplicate are sentinel errors used only to short-circuit out
// of withTx's transaction (they trigger a Rollback, not a retry — see the
// sqlBooker doc comment).
var (
	errSoldOut   = errors.New("sold out")
	errDuplicate = errors.New("duplicate request")
)

// Reserve books one vehicle in classCode at loc for [pickupDate, returnDate),
// atomically guarding availability across EVERY night in that range with a
// single conditional multi-row UPDATE (the SQL-native analog of Hotel Sim's
// findOneAndUpdate guard, extended to a date range the way Hotel Sim's own
// multi-night booking already works) — this is the genuinely PostgreSQL/
// relational-native technique this app demonstrates that Airline Sim's
// single-date model has no equivalent of. RowsAffected is checked against the
// number of nights: if EVERY night's row matched (available_vehicles>=1,
// not closed), the count matches exactly and the whole range is held in one
// statement; if even one night in the middle of the range lacked availability,
// that night's row fails the WHERE clause, the count falls short, and the
// entire transaction rolls back — no per-night loop, no partial hold, no
// compensation logic. De-duplicates on requestID via a unique key — a
// resubmitted requestID always returns ResultDuplicate rather than
// double-booking, whether it's retried by the same agent tick or arrives
// concurrently from two different connections. The specific vehicle instance is
// NOT assigned here — only at check-out (see CheckOut) — mirroring a real
// rental counter, where a class is booked well before a specific car is pulled
// from the lot.
func (b *sqlBooker) Reserve(ctx context.Context, loc *Location, classCode VehicleClassCode, pickupDate, returnDate time.Time, dropoffLocationID, renterID, renterName, requestID, agent string) (*Reservation, BookResult, error) {
	e := b.e
	nights := int(returnDate.Sub(pickupDate).Hours() / 24)
	if nights < 1 {
		nights = 1
		returnDate = pickupDate.AddDate(0, 0, 1)
	}
	var res *Reservation
	result := ResultOK

	err := b.withTx(ctx, func(tx *sql.Tx) error {
		result = ResultOK
		r, err := tx.ExecContext(ctx,
			"UPDATE rental_inventory SET booked_vehicles=booked_vehicles+1, available_vehicles=available_vehicles-1 "+
				"WHERE location_id=$1 AND class_code=$2 AND date>=$3 AND date<$4 AND available_vehicles>=1 AND closed=false",
			loc.ID, string(classCode), pickupDate.Format("2006-01-02"), returnDate.Format("2006-01-02"))
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		if int(n) != nights {
			result = ResultSoldOut
			return errSoldOut
		}

		if _, err := tx.ExecContext(ctx, "INSERT INTO reservation_requests (request_id, created_at) VALUES ($1,$2)", requestID, time.Now().UTC()); err != nil {
			if isDuplicateKey(err) {
				result = ResultDuplicate
				return errDuplicate
			}
			return err
		}

		var rateTotal float64
		if err := tx.QueryRowContext(ctx,
			"SELECT COALESCE(SUM(rate),0) FROM rental_inventory WHERE location_id=$1 AND class_code=$2 AND date>=$3 AND date<$4",
			loc.ID, string(classCode), pickupDate.Format("2006-01-02"), returnDate.Format("2006-01-02")).Scan(&rateTotal); err != nil {
			return err
		}
		rateTotal = round2(rateTotal)

		id := NewConfirmationNumber(loc.ID, pickupDate, e.nextConfirmationSeq())
		now := time.Now().UTC()
		hist := []HistoryEntry{{At: now, SimAt: e.Clock.Now(), Action: "booked", By: agent}}
		histJSON, _ := json.Marshal(hist)
		res = &Reservation{
			ID: id, RequestID: requestID, RenterID: renterID, RenterName: renterName,
			PickupLocationID: loc.ID, DropoffLocationID: dropoffLocationID, Region: loc.Region,
			ClassCode: classCode, PickupDate: pickupDate, ReturnDate: returnDate,
			RateTotal: rateTotal, Currency: "USD", Status: StatusConfirmed, Version: 1,
			History: hist, CreatedAt: now, UpdatedAt: now,
		}
		_, err = tx.ExecContext(ctx,
			"INSERT INTO reservations (id, request_id, renter_id, renter_name, pickup_location_id, dropoff_location_id, region, class_code, pickup_date, return_date, rate_total, currency, status, version, history, created_at, updated_at) "+
				"VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)",
			res.ID, res.RequestID, res.RenterID, res.RenterName, res.PickupLocationID, res.DropoffLocationID,
			string(res.Region), string(res.ClassCode), res.PickupDate.Format("2006-01-02"), res.ReturnDate.Format("2006-01-02"),
			res.RateTotal, res.Currency, string(res.Status), res.Version, string(histJSON), res.CreatedAt, res.UpdatedAt)
		return err
	})

	switch {
	case errors.Is(err, errSoldOut):
		e.counters.soldOut.Add(1)
		return nil, ResultSoldOut, nil
	case errors.Is(err, errDuplicate):
		e.counters.duplicatesRejected.Add(1)
		return nil, ResultDuplicate, nil
	case err != nil:
		return nil, "", err
	}

	e.counters.reservationsTotal.Add(1)
	e.rememberReservation(ResRef{ID: res.ID, LocationID: res.PickupLocationID, PickupDate: res.PickupDate, ReturnDate: res.ReturnDate})
	e.PublishEvent(ctx, "booked", res.ID, res.PickupLocationID, res.PickupDate, agent, fmt.Sprintf("%s x%d night(s)", classCode, nights))
	return res, result, nil
}

// releaseRange is Reserve's inverse: releases one vehicle's hold across
// [pickupDate, returnDate) back to rental_inventory. Shared by Cancel and
// ModifyDates/ModifyClass/ModifyDropoff's "give back the old range" step.
func releaseRange(ctx context.Context, tx *sql.Tx, locationID string, classCode VehicleClassCode, pickupDate, returnDate time.Time) error {
	_, err := tx.ExecContext(ctx,
		"UPDATE rental_inventory SET booked_vehicles=booked_vehicles-1, available_vehicles=available_vehicles+1 "+
			"WHERE location_id=$1 AND class_code=$2 AND date>=$3 AND date<$4",
		locationID, string(classCode), pickupDate.Format("2006-01-02"), returnDate.Format("2006-01-02"))
	return err
}

// Cancel cancels a confirmed reservation and releases its whole date range back
// to rental_inventory, in one transaction — no cross-node compensation needed,
// since it's all one relational database on every topology this app can be
// linked to. Only reachable from StatusConfirmed (a reservation already
// checked out has a real vehicle claimed and out the door — the fleet-ops
// analog of an airline no-show, not a cancellation).
func (b *sqlBooker) Cancel(ctx context.Context, resID, agent string) error {
	e := b.e
	err := b.withTx(ctx, func(tx *sql.Tx) error {
		var locationID, classCode, status string
		var pickupDate, returnDate time.Time
		err := tx.QueryRowContext(ctx, "SELECT pickup_location_id, class_code, pickup_date, return_date, status FROM reservations WHERE id=$1 FOR UPDATE", resID).
			Scan(&locationID, &classCode, &pickupDate, &returnDate, &status)
		if err != nil {
			return err
		}
		if status != string(StatusConfirmed) {
			return errSoldOut // reused as a generic "guard failed, don't retry" sentinel
		}
		r, err := tx.ExecContext(ctx,
			"UPDATE reservations SET status=$1, version=version+1, updated_at=$2 WHERE id=$3 AND status=$4",
			string(StatusCancelled), time.Now().UTC(), resID, status)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return errSoldOut
		}
		return releaseRange(ctx, tx, locationID, VehicleClassCode(classCode), pickupDate, returnDate)
	})
	if errors.Is(err, errSoldOut) {
		return nil // already terminal / race lost — not an error worth surfacing to the agent
	}
	if err != nil {
		return err
	}
	e.counters.cancellationsTotal.Add(1)
	var locationID string
	var pickupDate time.Time
	e.Store.DB.QueryRowContext(ctx, "SELECT pickup_location_id, pickup_date FROM reservations WHERE id=$1", resID).Scan(&locationID, &pickupDate)
	e.PublishEvent(ctx, "cancelled", resID, locationID, pickupDate, agent, "cancelled")
	return nil
}

// sweepNoShows flips up to limit still-confirmed reservations whose pickup date
// is on or before cutoff to no_show, releasing each one's held date range back
// to rental_inventory in the same transaction — a per-row loop (not Airline
// Sim's single bulk UPDATE) specifically because that release step needs each
// row's location/class/date range individually. Best-effort per row: one
// failure doesn't abort the rest of the batch.
func (e *Engine) sweepNoShows(ctx context.Context, cutoff time.Time, limit int) {
	rows, err := e.Store.DB.QueryContext(ctx,
		"SELECT id, pickup_location_id, class_code, pickup_date, return_date FROM reservations WHERE status=$1 AND pickup_date<=$2 LIMIT $3",
		string(StatusConfirmed), cutoff.Format("2006-01-02"), limit)
	if err != nil {
		return
	}
	type due struct {
		id, locationID, classCode string
		pickupDate, returnDate    time.Time
	}
	var candidates []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.id, &d.locationID, &d.classCode, &d.pickupDate, &d.returnDate) == nil {
			candidates = append(candidates, d)
		}
	}
	rows.Close()

	for _, d := range candidates {
		err := e.booker.withTx(ctx, func(tx *sql.Tx) error {
			now := time.Now().UTC()
			hist := []HistoryEntry{{At: now, SimAt: e.Clock.Now(), Action: "no_show", By: "check-in-agent"}}
			histJSON, _ := json.Marshal(hist)
			r, err := tx.ExecContext(ctx,
				"UPDATE reservations SET status=$1, history=history || $2::jsonb, updated_at=$3 WHERE id=$4 AND status=$5",
				string(StatusNoShow), string(histJSON), now, d.id, string(StatusConfirmed))
			if err != nil {
				return err
			}
			if n, _ := r.RowsAffected(); n == 0 {
				return nil // already changed underneath us — not an error
			}
			return releaseRange(ctx, tx, d.locationID, VehicleClassCode(d.classCode), d.pickupDate, d.returnDate)
		})
		if err == nil {
			e.counters.noShowsTotal.Add(1)
		}
	}
}

// ModifyDates extends or shortens a confirmed reservation's return date by
// deltaDays (+/-), guarded by the same available-vehicles conditional update as
// Reserve when extending. Reports whether the change was applied.
func (b *sqlBooker) ModifyDates(ctx context.Context, resID string, deltaDays int, agent string) (bool, error) {
	e := b.e
	applied := false
	err := b.withTx(ctx, func(tx *sql.Tx) error {
		applied = false
		var locationID, classCode, status string
		var pickupDate, returnDate time.Time
		err := tx.QueryRowContext(ctx, "SELECT pickup_location_id, class_code, pickup_date, return_date, status FROM reservations WHERE id=$1 FOR UPDATE", resID).
			Scan(&locationID, &classCode, &pickupDate, &returnDate, &status)
		if err != nil {
			return err
		}
		newReturn := returnDate.AddDate(0, 0, deltaDays)
		if status != string(StatusConfirmed) || !newReturn.After(pickupDate) {
			return nil // not an error — just not applicable right now
		}
		if deltaDays > 0 {
			r, err := tx.ExecContext(ctx,
				"UPDATE rental_inventory SET booked_vehicles=booked_vehicles+1, available_vehicles=available_vehicles-1 "+
					"WHERE location_id=$1 AND class_code=$2 AND date>=$3 AND date<$4 AND available_vehicles>=1 AND closed=false",
				locationID, classCode, returnDate.Format("2006-01-02"), newReturn.Format("2006-01-02"))
			if err != nil {
				return err
			}
			if n, _ := r.RowsAffected(); int(n) != deltaDays {
				return nil // sold out on the extra night(s) — leave the reservation untouched
			}
		} else if deltaDays < 0 {
			if err := releaseRange(ctx, tx, locationID, VehicleClassCode(classCode), newReturn, returnDate); err != nil {
				return err
			}
		}
		var rateTotal float64
		if err := tx.QueryRowContext(ctx,
			"SELECT COALESCE(SUM(rate),0) FROM rental_inventory WHERE location_id=$1 AND class_code=$2 AND date>=$3 AND date<$4",
			locationID, classCode, pickupDate.Format("2006-01-02"), newReturn.Format("2006-01-02")).Scan(&rateTotal); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE reservations SET return_date=$1, rate_total=$2, version=version+1, updated_at=$3 WHERE id=$4",
			newReturn.Format("2006-01-02"), round2(rateTotal), time.Now().UTC(), resID); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if applied {
		e.counters.modificationsTotal.Add(1)
	}
	return applied, nil
}

// ModifyDropoff changes a confirmed reservation's drop-off location (turning it
// into, or out of, a one-way rental) — no inventory impact, since drop-off
// location only affects which location the vehicle physically lands at on
// check-in (see CheckIn), not pickup-side capacity.
func (b *sqlBooker) ModifyDropoff(ctx context.Context, resID, newDropoffLocationID, agent string) (bool, error) {
	e := b.e
	res, err := e.Store.DB.ExecContext(ctx,
		"UPDATE reservations SET dropoff_location_id=$1, version=version+1, updated_at=$2 WHERE id=$3 AND status=$4",
		newDropoffLocationID, time.Now().UTC(), resID, string(StatusConfirmed))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		e.counters.modificationsTotal.Add(1)
		return true, nil
	}
	return false, nil
}

// CheckOut claims one SPECIFIC available vehicle for a confirmed reservation and
// flips it to checked_out — the genuinely PostgreSQL-idiomatic technique this
// app demonstrates that has no clean MySQL equivalent: `SELECT ... FOR UPDATE
// SKIP LOCKED` lets concurrent check-out agents each grab a different vehicle
// from the same location+class pool without blocking on each other or double-
// assigning the same VIN — a queue-claiming pattern MySQL requires an explicit
// decrement-guard workaround for instead. Reserve deliberately never assigns a
// vehicle instance; this is the only place one is claimed, mirroring a real
// rental counter pulling a specific car from the lot only once the renter
// actually shows up.
func (b *sqlBooker) CheckOut(ctx context.Context, resID, agent string) (bool, error) {
	e := b.e
	var claimedVIN string
	err := b.withTx(ctx, func(tx *sql.Tx) error {
		var locationID, classCode, status string
		err := tx.QueryRowContext(ctx, "SELECT pickup_location_id, class_code, status FROM reservations WHERE id=$1 FOR UPDATE", resID).
			Scan(&locationID, &classCode, &status)
		if err != nil {
			return err
		}
		if status != string(StatusConfirmed) {
			return errSoldOut
		}
		err = tx.QueryRowContext(ctx,
			"SELECT vin FROM vehicles WHERE current_location_id=$1 AND class_code=$2 AND status=$3 "+
				"FOR UPDATE SKIP LOCKED LIMIT 1",
			locationID, classCode, string(VehicleAvailable)).Scan(&claimedVIN)
		if err != nil {
			if err == sql.ErrNoRows {
				return errSoldOut // fleet exhausted at this location right now
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE vehicles SET status=$1, last_updated=$2 WHERE vin=$3",
			string(VehicleRented), time.Now().UTC(), claimedVIN); err != nil {
			return err
		}
		now := time.Now().UTC()
		hist := []HistoryEntry{{At: now, SimAt: e.Clock.Now(), Action: "checked_out", By: agent, From: string(StatusConfirmed), To: string(StatusCheckedOut)}}
		histJSON, _ := json.Marshal(hist)
		r, err := tx.ExecContext(ctx,
			"UPDATE reservations SET status=$1, vehicle_vin=$2, actual_check_out=$3, version=version+1, "+
				"history=history || $4::jsonb, updated_at=$5 WHERE id=$6 AND status=$7",
			string(StatusCheckedOut), claimedVIN, now, string(histJSON), now, resID, status)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return errSoldOut
		}
		return nil
	})
	if errors.Is(err, errSoldOut) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	e.setVehicleStatus(claimedVIN, VehicleRented)
	e.counters.checkOutsTotal.Add(1)
	var locationID string
	var pickupDate time.Time
	e.Store.DB.QueryRowContext(ctx, "SELECT pickup_location_id, pickup_date FROM reservations WHERE id=$1", resID).Scan(&locationID, &pickupDate)
	e.PublishEvent(ctx, "checked_out", resID, locationID, pickupDate, agent, "vehicle "+claimedVIN)
	return true, nil
}

// CheckIn returns a checked-out reservation's vehicle: flips the reservation to
// checked_in (terminal), and moves the vehicle to `cleaning` at whichever
// location it's actually being returned to (DropoffLocationID) — a genuine
// multi-table state change per return, not just a status flip, since a one-way
// rental (DropoffLocationID != PickupLocationID) means the vehicle now
// physically lives somewhere else in the fleet.
func (b *sqlBooker) CheckIn(ctx context.Context, resID, agent string) (bool, error) {
	e := b.e
	var vin, dropoffLocationID string
	err := b.withTx(ctx, func(tx *sql.Tx) error {
		var status string
		err := tx.QueryRowContext(ctx, "SELECT vehicle_vin, dropoff_location_id, status FROM reservations WHERE id=$1 FOR UPDATE", resID).
			Scan(&vin, &dropoffLocationID, &status)
		if err != nil {
			return err
		}
		if status != string(StatusCheckedOut) {
			return errSoldOut
		}
		now := time.Now().UTC()
		hist := []HistoryEntry{{At: now, SimAt: e.Clock.Now(), Action: "checked_in", By: agent, From: string(StatusCheckedOut), To: string(StatusCheckedIn)}}
		histJSON, _ := json.Marshal(hist)
		r, err := tx.ExecContext(ctx,
			"UPDATE reservations SET status=$1, actual_check_in=$2, version=version+1, "+
				"history=history || $3::jsonb, updated_at=$4 WHERE id=$5 AND status=$6",
			string(StatusCheckedIn), now, string(histJSON), now, resID, status)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return errSoldOut
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE vehicles SET status=$1, current_location_id=$2, last_updated=$3 WHERE vin=$4",
			string(VehicleCleaning), dropoffLocationID, now, vin); err != nil {
			return err
		}
		return nil
	})
	if errors.Is(err, errSoldOut) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	e.setVehicleStatus(vin, VehicleCleaning)
	e.setVehicleLocation(vin, dropoffLocationID)
	e.counters.checkInsTotal.Add(1)
	var pickupDate time.Time
	e.Store.DB.QueryRowContext(ctx, "SELECT pickup_date FROM reservations WHERE id=$1", resID).Scan(&pickupDate)
	e.PublishEvent(ctx, "checked_in", resID, dropoffLocationID, pickupDate, agent, "vehicle "+vin)
	return true, nil
}
