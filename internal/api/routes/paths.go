package routes

// HTTP route paths the daemon mounts. Kept here so the server's mux
// wiring and any client that wants to make a request (or a test that
// wants to assert against a known path) agree on the same strings.
const (
	PathHealth           = "/api/health"
	PathDoctor           = "/api/doctor"
	PathNow              = "/api/now"
	PathEvents           = "/api/events"
	PathEventsLevels     = "/api/events/levels"
	PathDebug            = "/api/debug"
	PathSessions         = "/api/sessions"
	PathSessionsPrefix   = "/api/sessions/"
	PathHistory          = "/api/history"
	PathCapacity         = "/api/capacity"
	PathAttribution      = "/api/attribution"
	PathBudgets          = "/api/budgets"
	PathModels           = "/api/models"
	PathPricesHeal       = "/api/prices/heal"
	PathCompactions      = "/api/compactions"
	PathLeaks            = "/api/leaks"
	PathSettings         = "/api/settings"
	PathExtractorRetrain = "/api/extractor/retrain"
	PathTrail            = "/api/trail"
	PathTrailResolve     = "/api/trail/resolve"
	PathAccounts         = "/api/accounts"
)
