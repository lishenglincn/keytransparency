// Copyright 2016 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package integration

import (
	"context"
	"database/sql"
	"encoding/pem"
	"fmt"
	"net"
	"time"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/any"
	"github.com/kr/pretty"
	"google.golang.org/grpc"

	"github.com/google/keytransparency/core/adminserver"
	"github.com/google/keytransparency/core/client"
	"github.com/google/keytransparency/core/integration"
	"github.com/google/keytransparency/core/keyserver"
	"github.com/google/keytransparency/core/mutator"
	"github.com/google/keytransparency/core/mutator/entry"
	"github.com/google/keytransparency/core/sequencer"
	"github.com/google/keytransparency/impl/authentication"
	"github.com/google/keytransparency/impl/authorization"
	"github.com/google/keytransparency/impl/sql/domain"
	"github.com/google/keytransparency/impl/sql/mutationstorage"
	"github.com/google/trillian/crypto/keys/der"
	"github.com/google/trillian/crypto/keyspb"
	"github.com/google/trillian/monitoring"
	"github.com/google/trillian/storage/testdb"

	pb "github.com/google/keytransparency/core/api/v1/keytransparency_go_proto"
	ttest "github.com/google/trillian/testonly/integration"

	_ "github.com/google/trillian/merkle/coniks"  // Register hasher
	_ "github.com/google/trillian/merkle/rfc6962" // Register hasher
	_ "github.com/mattn/go-sqlite3"               // Use sqlite database for testing.
)

var (
	// openssl ecparam -name prime256v1 -genkey
	vrfPriv = `-----BEGIN EC PRIVATE KEY-----
MHcCAQEEINS30QIDiMV1Npc8/J4OVGcHdSJTsiHhUx9rsK+OdLh2oAoGCCqGSM49
AwEHoUQDQgAEF2Pm2kKya+JBun1QRmKQMcoMOIBNWp8fjECkJX+/hNWdV1UKb12W
+yXcX2MqN7ZMX77hS9mLus/WaE0NS370mA==
-----END EC PRIVATE KEY-----`
	logPriv = `-----BEGIN EC PRIVATE KEY-----
MHcCAQEEIH7vjXeafneG0+7UxF1YGi4Env2L5LLnhqhfcwZafirMoAoGCCqGSM49
AwEHoUQDQgAEqGXPnhMIclRmYHSmAnCMmfDUJ9iNBMmFxR/wHJdL12AuVUkgcuhb
Ep2hy5ETs7bfFc2P95IYFlmbiuHMq3UY/A==
-----END EC PRIVATE KEY-----`
	mapPriv = `-----BEGIN EC PRIVATE KEY-----
MHcCAQEEIC4FhRijqobFJXcyojcPZX88sDtHgzp5ydmSgv1PqIlvoAoGCCqGSM49
AwEHoUQDQgAEWLHm0TLYaTzENpPkBl2E79ySqJI+EW51VpoWh7wqY3OjSJcft4zg
EeNeHYEb/T2jBFH4eYg4iSN7D/VYaJxJRA==
-----END EC PRIVATE KEY-----`
)

// Listen opens a random local port and listens on it.
func Listen() (string, net.Listener, error) {
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return "", nil, fmt.Errorf("failed to listen: %v", err)
	}
	_, port, err := net.SplitHostPort(lis.Addr().String())
	if err != nil {
		return "", nil, fmt.Errorf("Failed to parse listener address: %v", err)
	}
	addr := "localhost:" + port
	return addr, lis, nil
}

// Env holds a complete testing environment for end-to-end tests.
type Env struct {
	*integration.Env
	mapEnv        *ttest.MapEnv
	logEnv        *ttest.LogEnv
	admin         *adminserver.Server
	grpcServer    *grpc.Server
	grpcCC        *grpc.ClientConn
	db            *sql.DB
	stopSequencer func()
}

func vrfKeyGen(ctx context.Context, spec *keyspb.Specification) (proto.Message, error) {
	return der.NewProtoFromSpec(spec)
}

func keyFromPEM(p string) *any.Any {
	block, _ := pem.Decode([]byte(p))
	k := &keyspb.PrivateKey{Der: block.Bytes}
	a, err := ptypes.MarshalAny(k)
	if err != nil {
		panic("MarshalAny failed")
	}
	return a
}

// NewEnv sets up common resources for tests.
func NewEnv(ctx context.Context) (*Env, error) {
	timeout := 6 * time.Second
	domainID := "integration"

	db, err := testdb.NewTrillianDB(ctx)
	if err != nil {
		return nil, fmt.Errorf("env: failed to open database: %v", err)
	}

	// Map server
	mapEnv, err := ttest.NewMapEnv(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("env: failed to create trillian map server: %v", err)
	}

	// Log server
	numSequencers := 1
	unused := ""
	logEnv, err := ttest.NewLogEnv(ctx, numSequencers, unused)
	if err != nil {
		return nil, fmt.Errorf("env: failed to create trillian log server: %v", err)
	}

	// Configure domain, which creates new map and log trees.
	domainStorage, err := domain.NewStorage(db)
	if err != nil {
		return nil, fmt.Errorf("env: failed to create domain storage: %v", err)
	}
	adminSvr := adminserver.New(logEnv.Log, mapEnv.Map, logEnv.Admin, mapEnv.Admin, domainStorage, vrfKeyGen)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	domainPB, err := adminSvr.CreateDomain(cctx, &pb.CreateDomainRequest{
		DomainId:      domainID,
		MinInterval:   ptypes.DurationProto(100 * time.Millisecond),
		MaxInterval:   ptypes.DurationProto(60 * time.Hour),
		VrfPrivateKey: keyFromPEM(vrfPriv),
		LogPrivateKey: keyFromPEM(logPriv),
		MapPrivateKey: keyFromPEM(mapPriv),
	})
	if err != nil {
		return nil, fmt.Errorf("env: CreateDomain(): %v", err)
	}
	glog.V(5).Infof("Domain: %# v", pretty.Formatter(domainPB))

	// Common data structures.
	mutations, err := mutationstorage.New(db)
	if err != nil {
		return nil, fmt.Errorf("env: Failed to create mutations object: %v", err)
	}
	authFunc := authentication.FakeAuthFunc
	authz := &authorization.AuthzPolicy{}

	queue := mutator.MutationQueue(mutations)
	server := keyserver.New(logEnv.Log, mapEnv.Map, logEnv.Admin, mapEnv.Admin,
		entry.New(), domainStorage, queue, mutations)
	gsvr := grpc.NewServer(
		grpc.UnaryInterceptor(
			authorization.UnaryServerInterceptor(map[string]authorization.AuthPair{
				"/google.keytransparency.v1.KeyTransparency/UpdateEntry": {
					AuthnFunc: authFunc,
					AuthzFunc: authz.Authorize,
				},
			}),
		),
	)
	pb.RegisterKeyTransparencyServer(gsvr, server)

	// Sequencer Server.
	sequencerServer := sequencer.NewServer(
		domainStorage,
		logEnv.Admin, mapEnv.Admin,
		logEnv.Log, mapEnv.Map,
		mutations, mutations,
		monitoring.InertMetricFactory{},
	)

	sequencerClient, stop, err := sequencer.RunAndConnect(ctx, sequencerServer)
	if err != nil {
		return nil, fmt.Errorf("error launching sequencer server: %v", err)
	}

	// Serve and listen.
	addr, lis, err := Listen()
	if err != nil {
		return nil, fmt.Errorf("env: Listen(): %v", err)
	}
	go gsvr.Serve(lis)

	cc, err := grpc.Dial(addr, grpc.WithInsecure())
	if err != nil {
		return nil, fmt.Errorf("Dial(%v): %v", addr, err)
	}
	ktClient := pb.NewKeyTransparencyClient(cc)
	client, err := client.NewFromConfig(ktClient, domainPB)
	if err != nil {
		return nil, fmt.Errorf("NewFromConfig(): %v", err)
	}
	// Integration tests manually create epochs immediately, so retry fairly quickly.
	client.RetryDelay = 10 * time.Millisecond
	return &Env{
		Env: &integration.Env{
			Client:    client,
			Cli:       ktClient,
			Sequencer: sequencerClient,
			Domain:    domainPB,
			Timeout:   timeout,
			CallOpts: func(userID string) []grpc.CallOption {
				return []grpc.CallOption{grpc.PerRPCCredentials(authentication.GetFakeCredential(userID))}
			},
		},
		mapEnv:        mapEnv,
		logEnv:        logEnv,
		admin:         adminSvr,
		grpcServer:    gsvr,
		grpcCC:        cc,
		db:            db,
		stopSequencer: stop,
	}, nil
}

// Close releases resources allocated by NewEnv.
func (env *Env) Close() {
	env.stopSequencer()
	ctx := context.Background()
	if _, err := env.admin.DeleteDomain(ctx, &pb.DeleteDomainRequest{
		DomainId: env.Domain.DomainId,
	}); err != nil {
		glog.Errorf("env: Close(): DeleteDomain(%v): %v", env.Domain.DomainId, err)
	}
	env.grpcCC.Close()
	env.grpcServer.Stop()
	env.mapEnv.Close()
	env.logEnv.Close()
	env.db.Close()
}
