package fetcher

// SetBaseURLForTest overrides the DB-IP download base URL so black-box
// tests can point the fetcher at a stub httptest server. The file suffix
// `_test.go` keeps it out of production builds; the base URL has no
// config or exported surface (ADR 0003).
func (f *Fetcher) SetBaseURLForTest(u string) {
	f.baseURL = u
}
