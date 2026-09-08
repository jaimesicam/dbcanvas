package sim

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

// BookResult classifies a booking attempt's outcome for the agent that called it —
// mirrors Hotel Sim's three-way "ok"/"duplicate"/"sold out" result.
type BookResult string

const (
	ResultOK        BookResult = "ok"
	ResultDuplicate BookResult = "duplicate"
	ResultSoldOut   BookResult = "sold_out"
)

// sqlBooker is the ONE booking implementation used against every target family —
// unlike Hotel Sim's txnBooker/guardedBooker split (forced by MongoDB standalone
// lacking multi-document transactions), SQL gives real ACID transactions on every
// MySQL-family target this app can be linked to, including a lone standalone node.
// What actually varies by target is the failure mode a hot row surfaces under
// concurrent writers: ordinary InnoDB lock-wait deadlocks on `ps`/`mysql`, or a
// Galera certification conflict on `pxc` — both come back to the client as MySQL
// error 1213 ("Deadlock found when trying to get lock; try restarting transaction"),
// so one retry loop handles both uniformly. See withRetry.
type sqlBooker struct {
	e *Engine
}

// maxTxnRetries bounds how many times a single logical operation retries after a
// 1213. Retrying is required for correctness under Galera (the server does not
// retry a multi-statement transaction on the client's behalf), and is harmless
// overhead everywhere else.
const maxTxnRetries = 5

// withRetry runs fn inside a transaction, retrying the whole thing on a MySQL 1213
// (deadlock / Galera certification failure) with a small jittered backoff so
// concurrent retriers don't immediately re-collide.
func (b *sqlBooker) withRetry(ctx context.Context, fn func(tx *sql.Tx) error) error {
	var lastErr error
	for attempt := 0; attempt < maxTxnRetries; attempt++ {
		tx, err := b.e.Store.DB.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin: %w", err)
		}
		err = fn(tx)
		if err == nil {
			if cerr := tx.Commit(); cerr != nil {
				err = cerr
			} else {
				return nil
			}
		}
		tx.Rollback()
		if !isRetryable(err) {
			return err
		}
		lastErr = err
		b.e.counters.txnRetries.Add(1)
		time.Sleep(time.Duration(5+rand.Intn(20)) * time.Millisecond * time.Duration(attempt+1))
	}
	return fmt.Errorf("giving up after %d retries: %w", maxTxnRetries, lastErr)
}

func isRetryable(err error) bool {
	var me *mysqldriver.MySQLError
	if errors.As(err, &me) {
		return me.Number == 1213 || me.Number == 1205 // deadlock / lock wait timeout
	}
	return false
}

func isDuplicateKey(err error) bool {
	var me *mysqldriver.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// errSoldOut / errDuplicate are sentinel errors used only to short-circuit out of
// withRetry's transaction without triggering a retry (they're not itself a 1213).
var (
	errSoldOut   = errors.New("sold out")
	errDuplicate = errors.New("duplicate request")
)

// Reserve books `seats` seats in classCode on route's flightDate, atomically
// guarding availability with a single conditional UPDATE (the SQL-native analog of
// Hotel Sim's findOneAndUpdate guard) and de-duplicating on requestID via a unique
// key — a resubmitted requestID always returns ResultDuplicate rather than double-
// booking, whether it's retried by the same agent tick or arrives concurrently from
// two different connections.
func (b *sqlBooker) Reserve(ctx context.Context, route *Route, classCode SeatClassCode, flightDate time.Time, seats int, passengerID, passengerName, requestID, agent string) (*Reservation, BookResult, error) {
	e := b.e
	invID := InventoryID(route.ID, classCode, flightDate)
	var res *Reservation
	result := ResultOK

	err := b.withRetry(ctx, func(tx *sql.Tx) error {
		result = ResultOK
		r, err := tx.ExecContext(ctx,
			"UPDATE flight_inventory SET booked_seats=booked_seats+?, available_seats=available_seats-? WHERE id=? AND available_seats>=? AND closed=0",
			seats, seats, invID, seats)
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		if n == 0 {
			result = ResultSoldOut
			return errSoldOut
		}

		if _, err := tx.ExecContext(ctx, "INSERT INTO reservation_requests (request_id, created_at) VALUES (?, ?)", requestID, time.Now().UTC()); err != nil {
			if isDuplicateKey(err) {
				result = ResultDuplicate
				return errDuplicate
			}
			return err
		}

		var fare float64
		if err := tx.QueryRowContext(ctx, "SELECT fare FROM flight_inventory WHERE id=?", invID).Scan(&fare); err != nil {
			return err
		}
		fareTotal := round2(fare * float64(seats))

		id := NewConfirmationNumber(route.ID, flightDate, e.nextConfirmationSeq())
		now := time.Now().UTC()
		hist := []HistoryEntry{{At: now, SimAt: e.Clock.Now(), Action: "booked", By: agent}}
		histJSON, _ := json.Marshal(hist)
		res = &Reservation{
			ID: id, RequestID: requestID, PassengerID: passengerID, PassengerName: passengerName,
			RouteID: route.ID, FlightNumber: route.FlightNumber, Origin: route.Origin, Destination: route.Destination,
			Region: route.Region, ClassCode: classCode, FlightDate: flightDate, Seats: seats,
			FareTotal: fareTotal, Currency: "USD", Status: StatusConfirmed, Version: 1,
			History: hist, CreatedAt: now, UpdatedAt: now,
		}
		_, err = tx.ExecContext(ctx,
			"INSERT INTO reservations (id, request_id, passenger_id, passenger_name, route_id, flight_number, origin, destination, region, class_code, flight_date, seats, fare_total, currency, status, version, history, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			res.ID, res.RequestID, res.PassengerID, res.PassengerName, res.RouteID, res.FlightNumber, res.Origin, res.Destination,
			string(res.Region), string(res.ClassCode), res.FlightDate.Format("2006-01-02"), res.Seats, res.FareTotal, res.Currency,
			string(res.Status), res.Version, string(histJSON), res.CreatedAt, res.UpdatedAt)
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
	e.rememberReservation(ResRef{ID: res.ID, RouteID: res.RouteID, FlightDate: res.FlightDate})
	e.PublishEvent(ctx, "booked", res.ID, res.RouteID, res.FlightDate, agent, fmt.Sprintf("%s %d seat(s) %s", res.FlightNumber, res.Seats, classCode))
	return res, result, nil
}

// Cancel cancels a confirmed or checked-in reservation and releases its seats back
// to flight_inventory, in one transaction — no cross-node compensation needed, since
// it's all one relational database (unlike Hotel Sim's guarded-standalone saga path,
// which existed only because MongoDB standalone can't wrap a multi-document write in
// a real transaction at all).
func (b *sqlBooker) Cancel(ctx context.Context, resID, agent string) error {
	e := b.e
	err := b.withRetry(ctx, func(tx *sql.Tx) error {
		var routeID, classCode, status string
		var flightDate time.Time
		var seats int
		err := tx.QueryRowContext(ctx, "SELECT route_id, class_code, flight_date, seats, status FROM reservations WHERE id=? FOR UPDATE", resID).
			Scan(&routeID, &classCode, &flightDate, &seats, &status)
		if err != nil {
			return err
		}
		if status != string(StatusConfirmed) && status != string(StatusCheckedIn) {
			return errSoldOut // reused as a generic "guard failed, don't retry" sentinel
		}
		r, err := tx.ExecContext(ctx,
			"UPDATE reservations SET status=?, version=version+1, updated_at=? WHERE id=? AND status=?",
			string(StatusCancelled), time.Now().UTC(), resID, status)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return errSoldOut
		}
		invID := InventoryID(routeID, SeatClassCode(classCode), flightDate)
		_, err = tx.ExecContext(ctx,
			"UPDATE flight_inventory SET booked_seats=booked_seats-?, available_seats=available_seats+? WHERE id=?",
			seats, seats, invID)
		return err
	})
	if errors.Is(err, errSoldOut) {
		return nil // already terminal / race lost — not an error worth surfacing to the agent
	}
	if err != nil {
		return err
	}
	e.counters.cancellationsTotal.Add(1)
	var routeID string
	var flightDate time.Time
	e.Store.DB.QueryRowContext(ctx, "SELECT route_id, flight_date FROM reservations WHERE id=?", resID).Scan(&routeID, &flightDate)
	e.PublishEvent(ctx, "cancelled", resID, routeID, flightDate, agent, "cancelled")
	return nil
}

// ModifySeats adjusts a confirmed reservation's passenger count by delta (+/-),
// guarded by the same available-seats conditional update as Reserve when
// increasing. Reports whether the change was applied (false on sold-out or a status
// that no longer allows modification).
func (b *sqlBooker) ModifySeats(ctx context.Context, resID string, delta int, agent string) (bool, error) {
	e := b.e
	applied := false
	err := b.withRetry(ctx, func(tx *sql.Tx) error {
		applied = false
		var routeID, classCode, status string
		var flightDate time.Time
		var seats int
		var fareTotal float64
		err := tx.QueryRowContext(ctx, "SELECT route_id, class_code, flight_date, seats, fare_total, status FROM reservations WHERE id=? FOR UPDATE", resID).
			Scan(&routeID, &classCode, &flightDate, &seats, &fareTotal, &status)
		if err != nil {
			return err
		}
		if status != string(StatusConfirmed) || seats+delta < 1 {
			return nil // not an error — just not applicable right now
		}
		invID := InventoryID(routeID, SeatClassCode(classCode), flightDate)
		if delta > 0 {
			r, err := tx.ExecContext(ctx,
				"UPDATE flight_inventory SET booked_seats=booked_seats+?, available_seats=available_seats-? WHERE id=? AND available_seats>=?",
				delta, delta, invID, delta)
			if err != nil {
				return err
			}
			if n, _ := r.RowsAffected(); n == 0 {
				return nil // sold out — leave the reservation untouched
			}
		} else if delta < 0 {
			if _, err := tx.ExecContext(ctx,
				"UPDATE flight_inventory SET booked_seats=booked_seats+?, available_seats=available_seats-? WHERE id=?",
				delta, -delta, invID); err != nil {
				return err
			}
		}
		perSeat := fareTotal / float64(seats)
		newFare := round2(perSeat * float64(seats+delta))
		if _, err := tx.ExecContext(ctx,
			"UPDATE reservations SET seats=?, fare_total=?, version=version+1, updated_at=? WHERE id=?",
			seats+delta, newFare, time.Now().UTC(), resID); err != nil {
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

// Rebook moves a confirmed reservation to a different route/class/date: releases
// the old flight_inventory hold and claims a new one, in one transaction. Reports
// ResultSoldOut if the new flight has no room, leaving the original booking intact.
func (b *sqlBooker) Rebook(ctx context.Context, resID string, newRoute *Route, newClass SeatClassCode, newDate time.Time, agent string) (BookResult, error) {
	e := b.e
	result := ResultOK
	err := b.withRetry(ctx, func(tx *sql.Tx) error {
		result = ResultOK
		var oldRouteID, oldClass, status string
		var oldDate time.Time
		var seats int
		err := tx.QueryRowContext(ctx, "SELECT route_id, class_code, flight_date, seats, status FROM reservations WHERE id=? FOR UPDATE", resID).
			Scan(&oldRouteID, &oldClass, &oldDate, &seats, &status)
		if err != nil {
			return err
		}
		if status != string(StatusConfirmed) {
			result = ResultDuplicate // reused: "not applicable", nothing to retry
			return nil
		}
		newInvID := InventoryID(newRoute.ID, newClass, newDate)
		r, err := tx.ExecContext(ctx,
			"UPDATE flight_inventory SET booked_seats=booked_seats+?, available_seats=available_seats-? WHERE id=? AND available_seats>=? AND closed=0",
			seats, seats, newInvID, seats)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			result = ResultSoldOut
			return nil
		}
		oldInvID := InventoryID(oldRouteID, SeatClassCode(oldClass), oldDate)
		if _, err := tx.ExecContext(ctx,
			"UPDATE flight_inventory SET booked_seats=booked_seats-?, available_seats=available_seats+? WHERE id=?",
			seats, seats, oldInvID); err != nil {
			return err
		}
		var fare float64
		if err := tx.QueryRowContext(ctx, "SELECT fare FROM flight_inventory WHERE id=?", newInvID).Scan(&fare); err != nil {
			return err
		}
		newFare := round2(fare * float64(seats))
		_, err = tx.ExecContext(ctx,
			"UPDATE reservations SET route_id=?, flight_number=?, origin=?, destination=?, region=?, class_code=?, flight_date=?, fare_total=?, version=version+1, updated_at=? WHERE id=?",
			newRoute.ID, newRoute.FlightNumber, newRoute.Origin, newRoute.Destination, string(newRoute.Region), string(newClass),
			newDate.Format("2006-01-02"), newFare, time.Now().UTC(), resID)
		return err
	})
	if err != nil {
		return "", err
	}
	if result == ResultOK {
		e.counters.modificationsTotal.Add(1)
		e.PublishEvent(ctx, "rebooked", resID, newRoute.ID, newDate, agent, "rebooked to "+newRoute.FlightNumber)
	}
	return result, nil
}
