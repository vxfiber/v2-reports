package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/viper"
	"github.com/xuri/excelize/v2"
	"go.vxfiber.dev/proto-go/bss/accesspointpb"
	"go.vxfiber.dev/proto-go/bss/subscription/subscriptionpb"
	"go.vxfiber.dev/proto-go/iam/iampb"
	"go.vxfiber.dev/proto-go/netowner/installationpb"
	"go.vxfiber.dev/proto-go/netowner/workorderpb"
	"go.vxfiber.dev/vx-bouncer/sdk/auth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func formatAustrianTime(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return "-"
	}

	// Step 1: Convert to Go time.Time
	t := ts.AsTime()

	// Step 2: Load Austria/Vienna timezone
	loc, err := time.LoadLocation("Europe/Vienna")
	if err != nil {
		panic(fmt.Sprintf("could not load Europe/Vienna location: %v", err))
	}

	// Step 3: Convert to local time
	localTime := t.In(loc)

	// Step 4: Format (customize format as needed)
	return localTime.Format("2006-01-02 15:04:05 MST")
}

type Status string

const (
	Cancelled Status = "Cancelled"
	Aborted   Status = "Aborted"
	One       Status = "Provided to IKB"
	Two       Status = "Finished by IKB, ONT not sent"
	Three     Status = "Finished by IKB, ONT Sent"
	Four      Status = "Activated, ONT discovered"
)

type Csv struct {
	ServiceProviderReference string
	NetworkOwnerReference    string
	Status                   Status
	SubscriptionCreatedAt    string
	WorkOrderCompletedAt     string
	ONTSentAt                string
}

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

	woConn, err := grpc.NewClient("localhost:9999", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(auth.GrpcClientInterceptor))
	if err != nil {
		panic(err)
	}
	defer woConn.Close()

	woClient := workorderpb.NewWorkOrderServiceClient(woConn)

	wos, err := woClient.Get(ctx, &workorderpb.GetParameters{
		OrderBy:           workorderpb.OrderBy_ORDER_BY_CREATED_AT,
		OrderByDescending: false,
	})
	if err != nil {
		panic(err)
	}

	subConn, err := grpc.NewClient("localhost:9998", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(auth.GrpcClientInterceptor))
	if err != nil {
		panic(err)
	}
	defer subConn.Close()

	subClient := subscriptionpb.NewSubscriptionServiceClient(subConn)

	apConn, err := grpc.NewClient("localhost:9997", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(auth.GrpcClientInterceptor))
	if err != nil {
		panic(err)
	}
	defer apConn.Close()

	apClient := accesspointpb.NewAccessPointServiceClient(apConn)

	inClient := installationpb.NewInstallationServiceClient(woConn)

	csvs := make([]Csv, 0)
	for _, wo := range wos.WorkOrders {
		slog.Info("Processing work order", "id", wo.Id, "externalSubscriptionReference", wo.ExternalSubscriptionReference, "installationId", wo.InstallationId)
		sub, err := subClient.GetByID(ctx, &subscriptionpb.GetByIDParameters{
			Id: wo.ExternalSubscriptionReference,
		})
		if err != nil {
			panic(fmt.Sprintf("failed to get subscription by ID %s: %v", wo.ExternalSubscriptionReference, err))
		}
		ap, err := apClient.GetById(ctx, &accesspointpb.GetByIdParameters{
			Id: sub.AccesspointId,
		})
		if err != nil {
			panic(fmt.Sprintf("failed to get access point by ID %s: %v", sub.AccesspointId, err))
		}

		in, err := inClient.GetByID(ctx, &installationpb.GetByIDParameters{
			Id: wo.InstallationId,
		})
		if err != nil {
			panic(fmt.Sprintf("failed to get installation by ID %s: %v", wo.InstallationId, err))
		}

		c := Csv{
			ServiceProviderReference: sub.ExternalId,
			NetworkOwnerReference:    ap.AccessPoint.ExternalId,
			SubscriptionCreatedAt:    formatAustrianTime(sub.CreatedAt),
			WorkOrderCompletedAt:     formatAustrianTime(wo.EndedAt),
			ONTSentAt:                "-",
		}

		if wo.Status == workorderpb.Status_STATUS_CANCELLED {
			c.Status = Cancelled
			csvs = append(csvs, c)
			slog.Info("Work order cancelled, skipping further processing", "id", wo.Id)
			continue
		}

		if wo.Status == workorderpb.Status_STATUS_ABORTED {
			c.Status = Aborted
			csvs = append(csvs, c)
			slog.Info("Work order aborted, skipping further processing", "id", wo.Id)
			continue
		}

		var statusSet bool

		if in.Installation.Status == installationpb.Status_STATUS_COMPLETED {
			c.Status = Four
			statusSet = true
		}

		var ontSent bool
		for _, module := range in.Installation.Modules {
			if module.Name == installationpb.ModuleName_MODULE_NAME_ONT_SENDING {
				if module.Completed {
					ontSent = true
					c.ONTSentAt = formatAustrianTime(module.CompletedAt)
				}
			}
		}
		if ontSent && !statusSet {
			c.Status = Three
			statusSet = true
		}

		if wo.Status == workorderpb.Status_STATUS_COMPLETED && !statusSet {
			c.Status = Two
			statusSet = true
		}

		if !statusSet {
			c.Status = One
		}

		csvs = append(csvs, c)
	}

	// now write an a excel file with headers in the struct Csv
	file, err := os.Create("output.xlsx")
	if err != nil {
		panic(err)
	}
	defer file.Close()

	f := excelize.NewFile()
	sheetName := "Work Orders"
	index, err := f.NewSheet(sheetName)
	if err != nil {
		panic(err)
	}
	f.SetActiveSheet(index)
	f.SetCellValue(sheetName, "A1", "Service Provider Reference")
	f.SetCellValue(sheetName, "B1", "Network Owner Reference")
	f.SetCellValue(sheetName, "C1", "Status")
	f.SetCellValue(sheetName, "D1", "Subscription Created At")
	f.SetCellValue(sheetName, "E1", "Work Order Completed At")
	f.SetCellValue(sheetName, "F1", "ONT Sent At")
	for i, csv := range csvs {
		row := i + 2 // start from row 2 to leave space for headers
		f.SetCellValue(sheetName, fmt.Sprintf("A%d", row), csv.ServiceProviderReference)
		f.SetCellValue(sheetName, fmt.Sprintf("B%d", row), csv.NetworkOwnerReference)
		f.SetCellValue(sheetName, fmt.Sprintf("C%d", row), csv.Status)
		f.SetCellValue(sheetName, fmt.Sprintf("D%d", row), csv.SubscriptionCreatedAt)
		f.SetCellValue(sheetName, fmt.Sprintf("E%d", row), csv.WorkOrderCompletedAt)
		f.SetCellValue(sheetName, fmt.Sprintf("F%d", row), csv.ONTSentAt)
	}
	if err := f.SaveAs("output.xlsx"); err != nil {
		panic(fmt.Sprintf("failed to save file: %v", err))
	}
	fmt.Println("Excel file created successfully: output.xlsx")
	fmt.Println("You can open it with Excel or any compatible spreadsheet software.")
	fmt.Println("Done.")
	os.Exit(0)
}
