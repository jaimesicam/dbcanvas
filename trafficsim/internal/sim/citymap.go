package sim

import "fmt"

// A small, fixed, deterministic fictional city — a 3x3 grid of intersections (no
// real map data, per spec §5: deterministic and repeatable beats real-world
// accuracy for a demo). Coordinates are centered on an arbitrary base point with a
// small grid spacing so GEOADD/GEOSEARCH (valid longitude/latitude ranges) work on
// real Valkey geo commands, not just decoration.
const (
	gridSize   = 3
	baseLon    = -122.42
	baseLat    = 37.77
	cellDegLon = 0.006
	cellDegLat = 0.005
)

var vertNames = []string{"1st Ave", "2nd Ave", "3rd Ave"}
var horizNames = []string{"Elm St", "Oak St", "Pine St"}

// NewCityMap builds the fixed grid topology once at startup.
func NewCityMap() *CityMap {
	m := &CityMap{RoadByID: map[string]*Road{}, AdjOut: map[string][]*Road{}}

	ix := make([][]*Intersection, gridSize)
	for r := 0; r < gridSize; r++ {
		ix[r] = make([]*Intersection, gridSize)
		for c := 0; c < gridSize; c++ {
			it := &Intersection{
				ID:  fmt.Sprintf("ix-%d-%d", r, c),
				Row: r, Col: c,
				Lon:  baseLon + float64(c)*cellDegLon,
				Lat:  baseLat + float64(r)*cellDegLat,
				Name: fmt.Sprintf("%s & %s", horizNames[r], vertNames[c]),
			}
			ix[r][c] = it
			m.Intersections = append(m.Intersections, it)
		}
	}

	addRoad := func(from, to *Intersection, name string) {
		id := fmt.Sprintf("rd-%s-%s", from.ID, to.ID)
		road := &Road{
			ID: id, Name: name, From: from, To: to,
			SpeedLimit: 50, LengthM: 550, Lanes: 2,
			SignalID: "sig-" + to.ID,
		}
		m.Roads = append(m.Roads, road)
		m.RoadByID[id] = road
		m.AdjOut[from.ID] = append(m.AdjOut[from.ID], road)
	}

	// Horizontal roads (east/west along each row) — both directions.
	for r := 0; r < gridSize; r++ {
		for c := 0; c < gridSize-1; c++ {
			addRoad(ix[r][c], ix[r][c+1], horizNames[r])
			addRoad(ix[r][c+1], ix[r][c], horizNames[r])
		}
	}
	// Vertical roads (north/south along each column) — both directions.
	for c := 0; c < gridSize; c++ {
		for r := 0; r < gridSize-1; r++ {
			addRoad(ix[r][c], ix[r+1][c], vertNames[c])
			addRoad(ix[r+1][c], ix[r][c], vertNames[c])
		}
	}
	return m
}

// Signals returns one signal per intersection, all starting green — the signal
// agent staggers their cycles from there so they don't all change in lockstep.
func (m *CityMap) Signals() []*Signal {
	out := make([]*Signal, 0, len(m.Intersections))
	for _, it := range m.Intersections {
		out = append(out, &Signal{ID: "sig-" + it.ID, IntersectionID: it.ID, State: "green"})
	}
	return out
}

// LonLat interpolates a point along a road at position p (0..1), for GEOADD.
func (r *Road) LonLat(p float64) (float64, float64) {
	lon := r.From.Lon + (r.To.Lon-r.From.Lon)*p
	lat := r.From.Lat + (r.To.Lat-r.From.Lat)*p
	return lon, lat
}
