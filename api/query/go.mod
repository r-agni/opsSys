module github.com/systemscale/services/query-api

go 1.21

require github.com/systemscale/services/shared/auth v0.0.0

require (
	github.com/golang-jwt/jwt/v5 v5.2.1 // indirect
	github.com/lib/pq v1.11.2 // indirect
	golang.org/x/net v0.22.0 // indirect
	golang.org/x/sys v0.28.0 // indirect
	golang.org/x/text v0.21.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240318140521-94a12d6c2237 // indirect
	google.golang.org/grpc v1.64.0 // indirect
	google.golang.org/protobuf v1.34.2 // indirect
)

replace github.com/systemscale/services/shared/auth => ../shared/auth
