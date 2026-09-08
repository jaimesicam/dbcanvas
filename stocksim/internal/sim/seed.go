package sim

import (
	"context"
	"fmt"
	"time"

	"stocksim/internal/store"
)

// The starting universe. Small on purpose: this app is about watching CRUD and
// a report work against four different engines, not about volume. Twenty
// instruments and four accounts fit on one screen, seed in under a second even
// over a slow link, and still produce a report with something to say.

type seedSecurity struct {
	Symbol string
	Name   string
	Sector string
	Price  float64
	Shares int64
}

var seedSecurities = []seedSecurity{
	{"ACME", "Acme Industrial Corp", "Industrials", 182.40, 1_240_000_000},
	{"BRVO", "Bravo Semiconductor", "Technology", 411.05, 780_000_000},
	{"CDLA", "Cordelia Energy", "Energy", 64.18, 2_100_000_000},
	{"DELT", "Delta Grid Utilities", "Utilities", 97.32, 640_000_000},
	{"ECHO", "Echo Financial Group", "Financials", 148.77, 1_050_000_000},
	{"FXTR", "Foxtrot Retail Holdings", "Consumer", 33.51, 3_400_000_000},
	{"GLFR", "Golf River Pharma", "Healthcare", 226.90, 410_000_000},
	{"HTEL", "Hotel Continental Group", "Consumer", 58.04, 890_000_000},
	{"INDG", "Indigo Software", "Technology", 512.66, 320_000_000},
	{"JULT", "Juliett Materials", "Materials", 71.25, 1_180_000_000},
	{"KILO", "Kilo Logistics", "Industrials", 119.88, 540_000_000},
	{"LIMA", "Lima Foods", "Consumer", 44.63, 1_960_000_000},
	{"MIKE", "Mike Aerospace", "Industrials", 288.19, 260_000_000},
	{"NOVM", "November Telecom", "Communications", 26.74, 4_800_000_000},
	{"OSCR", "Oscar Biotech", "Healthcare", 92.55, 470_000_000},
	{"PAPA", "Papa Agricultural", "Materials", 38.90, 1_310_000_000},
	{"QBEC", "Quebec Insurance", "Financials", 165.42, 720_000_000},
	{"ROMO", "Romeo Motors", "Consumer", 204.17, 950_000_000},
	{"SIER", "Sierra Cloud Systems", "Technology", 347.83, 610_000_000},
	{"TANG", "Tango Shipping", "Industrials", 81.06, 830_000_000},
}

// Portfolio owners use the house sample names, not invented ones.
var seedPortfolios = []struct {
	Name  string
	Owner string
	Cash  float64
}{
	{"Growth Account", "Alice", 2_500_000},
	{"Balanced Account", "Bob", 1_800_000},
	{"Income Account", "Jane Doe", 3_200_000},
	{"Opportunistic Account", "John Doe", 950_000},
}

// SeedIfNeeded creates the schema and, if the securities table is empty, the
// starting universe. Idempotent: a redeploy against a database that already
// has data leaves that data alone, which is what makes the node's own restart
// path non-destructive.
func (e *Engine) SeedIfNeeded(ctx context.Context) error {
	e.setSeed(SeedProgress{Running: true, Step: "Creating schema", Percent: 5})

	if err := e.Store.EnsureSchema(ctx); err != nil {
		e.setSeed(SeedProgress{Done: true, Error: err.Error()})
		return err
	}

	e.setSeed(SeedProgress{Running: true, Step: "Checking for existing data", Percent: 15})
	existing, _, err := e.Store.ListSecurities(ctx, store.ListQuery{Limit: 1})
	if err != nil {
		e.setSeed(SeedProgress{Done: true, Error: err.Error()})
		return err
	}
	if len(existing) > 0 {
		e.setSeed(SeedProgress{Done: true, Step: "Existing data kept", Percent: 100})
		return nil
	}

	e.setSeed(SeedProgress{Running: true, Step: "Creating securities", Percent: 35})
	for _, s := range seedSecurities {
		if _, err := e.Store.CreateSecurity(ctx, store.Security{
			Symbol: s.Symbol, Name: s.Name, Sector: s.Sector, Currency: "USD",
			Shares: s.Shares, OpenPrice: s.Price, LastPrice: s.Price,
			DayHigh: s.Price, DayLow: s.Price, Listed: true,
		}); err != nil {
			e.setSeed(SeedProgress{Done: true, Error: err.Error()})
			return fmt.Errorf("seed security %s: %w", s.Symbol, err)
		}
	}

	e.setSeed(SeedProgress{Running: true, Step: "Creating portfolios", Percent: 70})
	for _, p := range seedPortfolios {
		if _, err := e.Store.CreatePortfolio(ctx, store.Portfolio{
			Name: p.Name, Owner: p.Owner, Cash: p.Cash,
		}); err != nil {
			e.setSeed(SeedProgress{Done: true, Error: err.Error()})
			return fmt.Errorf("seed portfolio %s: %w", p.Name, err)
		}
	}

	e.Store.AppendEvent(ctx, store.Event{
		TS: time.Now().UTC(), Kind: "system",
		Message: fmt.Sprintf("Seeded %d securities and %d portfolios",
			len(seedSecurities), len(seedPortfolios)),
	})
	e.setSeed(SeedProgress{Done: true, Step: "Ready", Percent: 100})
	return nil
}
