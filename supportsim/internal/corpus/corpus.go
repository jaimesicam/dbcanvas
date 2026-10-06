// Package corpus is the support desk's world: the teams, the knowledge-base
// articles they maintain, and the many ways a customer describes the same problem.
//
// The whole demo rests on one property of this data, so it is worth stating: a
// customer and a knowledge base do not use the same words. The article is called
// "Recovering access when you lose your MFA device"; the ticket says "got a new
// phone and the six-digit codes are gone". A keyword engine has almost nothing to
// match there. An embedding model has seen enough text to know those are the same
// situation. Every archetype below is written that way on purpose — official
// vocabulary in the article, customer vocabulary in the tickets — because that gap
// is the everyday reason vector search exists, and a demo whose tickets reuse the
// article's title would prove nothing.
//
// Each generated ticket carries its archetype as ground truth. A real help desk
// never has that; the simulator does, which is what lets the dashboard *score*
// keyword search and vector search on the same tickets instead of asserting that
// one is better.
package corpus

import (
	"fmt"
	"math/rand"
	"strings"
)

// Team is a support queue a ticket can be routed to.
type Team struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Teams, in display order. Colors are the dashboard's series colors.
var Teams = []Team{
	{"Billing", "#e8a33d"},
	{"Access & Identity", "#7c6cf0"},
	{"Performance", "#e5534b"},
	{"Backup & Restore", "#2f9e72"},
	{"Connectivity", "#3b82f6"},
	{"Replication & HA", "#c2410c"},
	{"Incident Response", "#db2777"},
}

// Article is one knowledge-base article — the thing a good answer links to.
type Article struct {
	Slug    string   `json:"slug" bson:"_id"`
	Title   string   `json:"title" bson:"title"`
	Team    string   `json:"team" bson:"team"`
	Summary string   `json:"summary" bson:"summary"`
	Steps   []string `json:"steps" bson:"steps"`
}

// EmbedText is what gets embedded for an article: its title and summary. The
// steps are left out on purpose — they are long, procedural, and full of words
// ("click", "settings", "save") that every article shares, which pulls every
// article's vector toward the same place.
func (a Article) EmbedText() string { return a.Title + ". " + a.Summary }

// Archetype is one kind of problem: the article that answers it and the ways
// customers phrase it.
type Archetype struct {
	ID       string
	Team     string
	KB       string   // slug of the answering article
	Subjects []string // subject lines, in customer vocabulary
	Details  []string // body sentences describing the situation
	Incident bool     // part of an outage burst rather than the everyday stream
}

// Ticket is a generated support ticket, before the desk has done anything to it.
type Ticket struct {
	Subject   string
	Body      string
	Customer  string
	Plan      string
	Archetype string // ground truth: which problem this really is
	TruthTeam string
	TruthKB   string
}

// EmbedText is what gets embedded for a ticket.
func (t Ticket) EmbedText() string { return t.Subject + ". " + t.Body }

// Plans a customer can be on — a filter field in the vector index.
var Plans = []string{"Starter", "Pro", "Business", "Enterprise"}

var customers = []string{
	"Lumen Analytics", "Harbor Freight Labs", "Quokka Games", "Bluefin Health", "Tidewater Logistics",
	"Northpeak Retail", "Copperleaf Bank", "Saffron Kitchens", "Vantage Robotics", "Juniper Schools",
	"Pixel Orchard", "Granite Insurance", "Mosaic Travel", "Ember Energy", "Atlas Mobility",
	"Kestrel Media", "Brightwater Labs", "Cobalt Payments", "Fernwood Pharmacy", "Orca Security",
	"Sundial Fitness", "Redwood Legal", "Polar Foods", "Nimbus Weather", "Ironclad Manufacturing",
}

var openers = []string{
	"Hi team,", "Hello,", "Hi there,", "Good morning,", "Hey support,", "Hi,", "", "", "",
}

var context = []string{
	"This is affecting production.", "We noticed it this morning.", "It started a couple of hours ago.",
	"Our on-call engineer flagged it.", "Not urgent, but we'd like to understand it.", "Our CTO is asking about this.",
	"It happens in our staging project too.", "We haven't changed anything on our side.", "Several users reported it.",
	"", "", "",
}

var closers = []string{
	"Thanks!", "Thanks in advance.", "Appreciate the help.", "Please advise.", "Any help is welcome.", "Cheers,", "", "",
}

// Generator produces an endless, varied stream of tickets.
type Generator struct {
	rng *rand.Rand
}

// NewGenerator returns a generator seeded for reproducible streams.
func NewGenerator(seed int64) *Generator { return &Generator{rng: rand.New(rand.NewSource(seed))} }

func (g *Generator) pick(xs []string) string { return xs[g.rng.Intn(len(xs))] }

// Everyday returns the archetypes of the normal ticket stream.
func Everyday() []Archetype {
	var out []Archetype
	for _, a := range Archetypes {
		if !a.Incident {
			out = append(out, a)
		}
	}
	return out
}

// Incidents returns the outage archetypes.
func Incidents() []Archetype {
	var out []Archetype
	for _, a := range Archetypes {
		if a.Incident {
			out = append(out, a)
		}
	}
	return out
}

// ArchetypeByID looks one up.
func ArchetypeByID(id string) (Archetype, bool) {
	for _, a := range Archetypes {
		if a.ID == id {
			return a, true
		}
	}
	return Archetype{}, false
}

// ArticleBySlug looks one up.
func ArticleBySlug(slug string) (Article, bool) {
	for _, a := range Articles {
		if a.Slug == slug {
			return a, true
		}
	}
	return Article{}, false
}

// Random returns a ticket from a random everyday archetype.
func (g *Generator) Random() Ticket {
	ev := Everyday()
	return g.From(ev[g.rng.Intn(len(ev))])
}

var prefixes = []string{"", "", "", "", "Urgent: ", "Question: ", "[prod] ", "Help — ", "Re: ", "Issue: "}
var suffixes = []string{"", "", "", "", " — please help", " (urgent)", "?", " again", " since this morning", " for our team"}

// From writes one ticket for an archetype: a subject, one or two detail
// sentences, and some of the noise real tickets carry (greetings, plan names,
// "this is affecting production") — noise a keyword engine has to wade through.
//
// The subject is sometimes one of the archetype's subject lines and sometimes its
// own first detail sentence, dressed with a prefix or suffix, so that a new ticket
// rarely repeats an old one word for word. Without that, "search the past tickets"
// would mostly be finding copies — which any engine can do — rather than finding
// the same problem in other words, which is the thing worth demonstrating.
func (g *Generator) From(a Archetype) Ticket {
	var body []string
	if o := g.pick(openers); o != "" {
		body = append(body, o)
	}
	d := g.rng.Perm(len(a.Details))
	subject := g.pick(a.Subjects)
	if g.rng.Float64() < 0.35 {
		subject = strings.TrimSuffix(a.Details[d[0]], ".")
		d = d[1:]
	}
	subject = g.pick(prefixes) + subject + g.pick(suffixes)
	n := 1 + g.rng.Intn(2)
	for i := 0; i < n && i < len(d); i++ {
		body = append(body, a.Details[d[i]])
	}
	if c := g.pick(context); c != "" {
		body = append(body, c)
	}
	if c := g.pick(closers); c != "" {
		body = append(body, c)
	}
	return Ticket{
		Subject:   subject,
		Body:      strings.Join(body, " "),
		Customer:  g.pick(customers),
		Plan:      Plans[g.rng.Intn(len(Plans))],
		Archetype: a.ID,
		TruthTeam: a.Team,
		TruthKB:   a.KB,
	}
}

// Validate checks the data's internal references; the tests call it.
func Validate() error {
	slugs := map[string]bool{}
	for _, a := range Articles {
		if slugs[a.Slug] {
			return fmt.Errorf("duplicate article %s", a.Slug)
		}
		slugs[a.Slug] = true
	}
	teams := map[string]bool{}
	for _, t := range Teams {
		teams[t.Name] = true
	}
	for _, a := range Archetypes {
		if !slugs[a.KB] {
			return fmt.Errorf("archetype %s: unknown article %s", a.ID, a.KB)
		}
		if !teams[a.Team] {
			return fmt.Errorf("archetype %s: unknown team %s", a.ID, a.Team)
		}
		if len(a.Subjects) < 3 || len(a.Details) < 2 {
			return fmt.Errorf("archetype %s: too few phrasings", a.ID)
		}
	}
	return nil
}
