// Package web embeds the gateway-served panel assets.
//
// The embed declaration is frozen in M0 (devops 必做项 / 测试 seq=265 "embed 声明行");
// serving /api/* routes and injecting window.__ALL2API_BASE__ is M1 (G5 ruling).
// The "all:" prefix is required: without it, dot-prefixed files such as
// .gitkeep are excluded and the placeholder dist would match zero files.
package web

import "embed"

// Dist holds the built panel assets (web-panel output copied into dist/).
// It stays an empty placeholder until the frontend build lands.
//
//go:embed all:dist
var Dist embed.FS
