// Package main is the entry point for the HotPlex Worker Gateway.
//
//	@title          HotPlex API
//	@version        1.52.0
//	@description    HotPlex Worker Gateway — unified access layer for AI Coding Agent sessions.
//	@contact.name   HotPlex
//	@contact.url    https://github.com/hrygo/hotplex/issues
//	@license.name   Apache 2.0
//	@license.url    https://github.com/hrygo/hotplex/blob/main/LICENSE
//
//	@host     localhost:8888
//	@BasePath /
//	@note     Admin API endpoints are served on port 9999; use the Scalar console server selector or import into Postman with port override.
//
//	@tag.name         Gateway API
//	@tag.description  Session management for end users (port 8888, header: X-Api-Key)
//	@tag.name         Admin API
//	@tag.description  Administrative endpoints (port 9999, header: Authorization Bearer <token>)
//
//	@securityDefinitions.apikey ApiKeyAuth
//	@in                         header
//	@name                       X-Api-Key
//	@description                API key for Gateway endpoints (port 8888)
//
//	@securityDefinitions.apikey AdminBearerAuth
//	@in                         header
//	@name                       Authorization
//	@description                Bearer token for Admin endpoints (port 9999). Format: "Bearer <token>"
package main
