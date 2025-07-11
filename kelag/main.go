package main

import (
	"context"
	"os"

	"github.com/spf13/viper"
	"go.vxfiber.dev/proto-go/iam/iampb"
	"go.vxfiber.dev/proto-go/inventory/devicepb"
	inventorypb "go.vxfiber.dev/proto-go/inventory/inventorypb"
	"go.vxfiber.dev/proto-go/inventory/poppb"
	"go.vxfiber.dev/vx-bouncer/sdk/auth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	ctx := context.Background()

	token := viper.GetString("vault-token")
	if token == "" {
		token = os.Getenv("VAULT_TOKEN") // we have prefix to the env var
	}

	if token == "" {
		panic("vault token is not set")
	}

	ctx = auth.WithAuth(ctx, &auth.Auth{
		Token: token,
	})

	foID := viper.GetString("fiber-operator-id")
	if foID == "" {
		foID = os.Getenv("FIBER_OPERATOR_ID") // we have prefix to the env var
	}

	ctx = auth.WithActor(ctx, &iampb.AuthActor{
		Type: iampb.AuthActor_TYPE_FIBER_OPERATOR,
		Id:   foID,
	})

	invConn, err := grpc.NewClient("localhost:9999", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(auth.GrpcClientInterceptor))
	if err != nil {
		panic(err)
	}
	defer invConn.Close()

	invClient := inventorypb.NewServiceClient(invConn)
	pops, err := invClient.GetPops(ctx, &poppb.GetPopParameters{})
	if err != nil {
		panic(err)
	}

	for _, pop := range pops.Pops {
		devices, err := invClient.GetDevices(ctx, &devicepb.GetDevicesParameters{
			PopId: &pop.Id,
		})
		if err != nil {
			panic(err)
		}
		for _, device := range devices.Devices {
			// Process each device
		}
	}
}
