package main

import "regexp"

func mustRe(s string) *regexp.Regexp { return regexp.MustCompile(s) }

// Realistic-data libraries used by the generators. Kept small but varied — combined
// with random numeric suffixes they give high enough cardinality for large datasets.

var firstNames = []string{
	"James", "Mary", "Robert", "Patricia", "John", "Jennifer", "Michael", "Linda", "David", "Elizabeth",
	"William", "Barbara", "Richard", "Susan", "Joseph", "Jessica", "Thomas", "Sarah", "Charles", "Karen",
	"Dana", "Devin", "Aisha", "Mateo", "Sofia", "Liam", "Noah", "Olivia", "Emma", "Ava",
	"Priya", "Wei", "Yuki", "Ingrid", "Omar", "Chen", "Fatima", "Hiro", "Lucas", "Nina",
}

var lastNames = []string{
	"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis", "Rodriguez", "Martinez",
	"Hernandez", "Lopez", "Gonzalez", "Wilson", "Anderson", "Thomas", "Taylor", "Moore", "Jackson", "Martin",
	"Adams", "Lopez", "Nguyen", "Kim", "Patel", "Singh", "Chen", "Wang", "Kowalski", "Rossi",
	"Andersson", "Okafor", "Yamamoto", "Silva", "Novak", "Haddad", "Ivanov", "Dubois", "Meyer", "Costa",
}

var cities = []string{
	"New York", "London", "Tokyo", "Paris", "Berlin", "Madrid", "Rome", "Toronto", "Sydney", "Mumbai",
	"São Paulo", "Cairo", "Lagos", "Seoul", "Singapore", "Amsterdam", "Dublin", "Vienna", "Prague", "Warsaw",
	"Austin", "Denver", "Chicago", "Boston", "Seattle", "Miami", "Lisbon", "Oslo", "Helsinki", "Nairobi",
}

var countries = []string{
	"United States", "United Kingdom", "Japan", "France", "Germany", "Spain", "Italy", "Canada", "Australia", "India",
	"Brazil", "Egypt", "Nigeria", "South Korea", "Singapore", "Netherlands", "Ireland", "Austria", "Poland", "Kenya",
}

var companies = []string{
	"Acme Corp", "Globex", "Initech", "Umbrella", "Hooli", "Stark Industries", "Wayne Enterprises", "Wonka",
	"Cyberdyne", "Soylent", "Vandelay", "Massive Dynamic", "Pied Piper", "Aperture", "Black Mesa", "Tyrell",
	"Nakatomi", "Weyland", "Oscorp", "Gringotts",
}

var jobTitles = []string{
	"Software Engineer", "Data Scientist", "Product Manager", "DBA", "DevOps Engineer", "QA Engineer",
	"Site Reliability Engineer", "Designer", "Analyst", "Architect", "Support Engineer", "Technical Writer",
	"Sales Engineer", "Account Manager", "CTO", "Engineering Manager",
}

var streets = []string{"Main St", "Oak Ave", "Maple Rd", "Cedar Ln", "Pine St", "Elm St", "Park Ave", "Lake Dr", "Hill Rd", "Sunset Blvd"}

var domains = []string{"example.com", "example.net", "test.org", "mail.com", "acme.io", "globex.co", "demo.dev"}

var statuses = []string{"active", "inactive", "pending", "archived", "suspended", "completed", "failed", "draft"}

var words = []string{
	"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet",
	"kilo", "lima", "mike", "november", "oscar", "papa", "quebec", "romeo", "sierra", "tango",
	"lorem", "ipsum", "dolor", "sit", "amet", "consectetur", "adipiscing", "elit", "sed", "tempor",
	"data", "cloud", "server", "cluster", "query", "index", "vector", "metric", "signal", "stream",
}
