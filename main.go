package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/skpr/yolog"
	"go-simpler.org/env"
)

// Config holds the environment configuration
type Config struct {
	StreamName     string `env:"STREAM_NAME,required"`
	TargetGroupARN string `env:"TARGET_GROUP_ARN,required"`
	LooupHost      string `env:"LOOKUP_HOST,required"` // keeping your original field name
}

const (
	targetPort           int32 = 443
	dnsLookupTimeout           = 2 * time.Second
	dnsLookupAttempts          = 3
	dnsLookupBackoffBase       = 75 * time.Millisecond
)

func main() {
	lambda.Start(handler)
}

func handler(ctx context.Context) error {
	var config Config
	if err := env.Load(&config, nil); err != nil {
		return err
	}

	logger := yolog.NewLogger(config.StreamName)
	defer logger.Log(os.Stdout)

	logger.SetAttrs("target_group", config.TargetGroupARN, "lookup_host", config.LooupHost)

	registerIPs, err := lookupIPv4StringsWithRetry(ctx, config.LooupHost)
	if err != nil {
		return logger.WrapError(err)
	}
	if len(registerIPs) == 0 {
		return logger.WrapError(fmt.Errorf("no A records (IPv4) for %s", config.LooupHost))
	}

	logger.SetAttrs("lookup_found_ips", registerIPs)

	awsConfig, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return logger.WrapError(err)
	}

	elb := elasticloadbalancingv2.NewFromConfig(awsConfig)

	targetHealth, err := elb.DescribeTargetHealth(ctx, &elasticloadbalancingv2.DescribeTargetHealthInput{
		TargetGroupArn: aws.String(config.TargetGroupARN),
	})
	if err != nil {
		return logger.WrapError(err)
	}

	targetsToRegister, targetsToDeregister := diffTargets(registerIPs, targetHealth.TargetHealthDescriptions, targetPort)

	logger.SetAttrs("register_targets", targetDescriptionToSlice(targetsToRegister))
	logger.SetAttrs("deregister_targets", targetDescriptionToSlice(targetsToDeregister))

	if len(targetsToDeregister) > 0 {
		_, err := elb.DeregisterTargets(ctx, &elasticloadbalancingv2.DeregisterTargetsInput{
			TargetGroupArn: aws.String(config.TargetGroupARN),
			Targets:        targetsToDeregister,
		})
		if err != nil {
			return logger.WrapError(err)
		}
	}

	if len(targetsToRegister) > 0 {
		_, err := elb.RegisterTargets(ctx, &elasticloadbalancingv2.RegisterTargetsInput{
			TargetGroupArn: aws.String(config.TargetGroupARN),
			Targets:        targetsToRegister,
		})
		if err != nil {
			return logger.WrapError(err)
		}
	}

	return nil
}

// lookupIPv4StringsWithRetry resolves A records (IPv4) with a short timeout and limited retries.
func lookupIPv4StringsWithRetry(parent context.Context, host string) ([]string, error) {
	var lastErr error

	for attempt := 0; attempt < dnsLookupAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(parent, dnsLookupTimeout)
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
		cancel()

		if err == nil {
			out := make([]string, 0, len(ips))

			for _, ip := range ips {
				out = append(out, ip.String())
			}

			return out, nil
		}

		lastErr = err

		// Retry only on temp/timeout DNS errors.
		var dnsErr *net.DNSError

		if errors.As(err, &dnsErr) && (dnsErr.IsTemporary || dnsErr.IsTimeout) {
			time.Sleep(dnsLookupBackoffBase * time.Duration(1<<attempt))
			continue
		}

		return nil, err
	}

	return nil, lastErr
}

// diffTargets computes:
//   - toRegister = desired(lookup) - current(existing)
//   - toDeregister = current(existing) - desired(lookup)
func diffTargets(lookup []string, existing []types.TargetHealthDescription, port int32) (toRegister, toDeregister []types.TargetDescription) {
	desired := make(map[string]struct{}, len(lookup))

	for _, ip := range lookup {
		desired[ip] = struct{}{}
	}

	for _, th := range existing {
		if th.Target == nil || th.Target.Id == nil {
			continue
		}

		id := *th.Target.Id

		if _, ok := desired[id]; ok {
			// Already registered and still desired.
			delete(desired, id)
			continue
		}

		// Registered but no longer desired.
		toDeregister = append(toDeregister, types.TargetDescription{
			Id:   th.Target.Id,
			Port: th.Target.Port,
		})
	}

	// Remaining desired are missing -> register them.
	toRegister = make([]types.TargetDescription, 0, len(desired))

	for ip := range desired {
		ip := ip

		toRegister = append(toRegister, types.TargetDescription{
			Id:   aws.String(ip),
			Port: aws.Int32(port),
		})
	}

	return toRegister, toDeregister
}

// Returns a slice of target IDs from a slice of TargetDescription
func targetDescriptionToSlice(targets []types.TargetDescription) []string {
	result := make([]string, 0, len(targets))

	for _, t := range targets {
		if t.Id != nil {
			result = append(result, *t.Id)
		}
	}

	return result
}
