package svc

import (
{{.imports}}
	// <<ENT_IMPORTS_BEGIN>>
	// <<ENT_IMPORTS_END>>
)

type ServiceContext struct {
	Config config.Config

	// ──── ent (auto-installed by zctl rpc ent / rpc dao) ────
	// <<ENT_FIELDS_BEGIN>>
	// <<ENT_FIELDS_END>>

	// ──── DAOs (auto-registered by zctl rpc ent / rpc dao) ────
	// <<DAOS_BEGIN>>
	// <<DAOS_END>>

	// ──── Repos (cache-aware, auto-registered by zctl rpc ent / rpc dao) ────
	// <<REPOS_BEGIN>>
	// <<REPOS_END>>
}

func NewServiceContext(c config.Config) *ServiceContext {
	// <<ENT_INIT_BEGIN>>
	// <<ENT_INIT_END>>

	// <<REPO_INFRA_BEGIN>>
	// <<REPO_INFRA_END>>

	return &ServiceContext{
		Config: c,
		// <<ENT_FIELDS_INIT_BEGIN>>
		// <<ENT_FIELDS_INIT_END>>

		// <<DAOS_INIT_BEGIN>>
		// <<DAOS_INIT_END>>

		// <<REPOS_INIT_BEGIN>>
		// <<REPOS_INIT_END>>
	}
}

// <<REDIS_HELPERS_BEGIN>>
// <<REDIS_HELPERS_END>>
