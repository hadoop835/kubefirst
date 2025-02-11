package domain

import (
	"reflect"
	"testing"
)

func TestNewTelemetry(t *testing.T) {

	clusterId := "894b39b0-1df7-44b3-a418-a36e298b55a6"
	clusterType := "mgmt"
	kubeFirstTeam := "false"
	validTelemetry := Telemetry{MetricName: "test metric", Domain: "example.com", CLIVersion: "0.0.0", ClusterId: clusterId, ClusterType: clusterType, KubeFirstTeam: kubeFirstTeam}

	type args struct {
		metricName    string
		domain        string
		cliVersion    string
		cloudProvider string
		gitProvider   string
		clusterId     string
		clusterType   string
		kubeFirstTeam string
	}
	tests := []struct {
		name    string
		args    args
		want    Telemetry
		wantErr bool
	}{
		{
			name: "valid domain",
			args: args{
				metricName:    "test metric",
				domain:        "https://example.com",
				cliVersion:    "0.0.0",
				cloudProvider: "local",
				gitProvider:   "github",
				clusterId:     clusterId,
				clusterType:   clusterType,
				kubeFirstTeam: kubeFirstTeam,
			},
			want:    validTelemetry,
			wantErr: false,
		},
		{
			name: "invalid domain",
			args: args{
				metricName:    "test metric",
				domain:        "https://example-com",
				cliVersion:    "0.0.0",
				cloudProvider: "local",
				gitProvider:   "github",
				clusterId:     clusterId,
				clusterType:   clusterType,
				kubeFirstTeam: kubeFirstTeam,
			},
			want:    Telemetry{},
			wantErr: true,
		},
		{
			name: "empty domain (localhost)",
			args: args{
				metricName:    "test metric",
				domain:        "",
				cliVersion:    "0.0.0",
				cloudProvider: "local",
				gitProvider:   "github",
				clusterId:     clusterId,
				clusterType:   clusterType,
				kubeFirstTeam: kubeFirstTeam,
			},
			want: Telemetry{
				MetricName:    "test metric",
				Domain:        clusterId,
				CloudProvider: "local",
				GitProvider:   "github",
				CLIVersion:    "0.0.0",
				ClusterId:     clusterId,
				ClusterType:   clusterType,
				KubeFirstTeam: kubeFirstTeam,
			},
			wantErr: false,
		},
		{
			name: "missing telemetry name",
			args: args{
				metricName:    "",
				domain:        "example.com",
				cliVersion:    "0.0.0",
				cloudProvider: "local",
				gitProvider:   "github",
			},
			want:    Telemetry{},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewTelemetry(tt.args.metricName, tt.args.domain, tt.args.cliVersion, tt.args.cloudProvider, tt.args.gitProvider, "894b39b0-1df7-44b3-a418-a36e298b55a6")
			if (err != nil) != tt.wantErr {
				t.Errorf("NewTelemetry() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NewTelemetry() got = %v, want %v", got, tt.want)
			}
		})
	}
}
