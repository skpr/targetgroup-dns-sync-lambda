package main

import (
	"context"
	"net"
	"os"
	"slices"

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
	LooupHost      string `env:"LOOKUP_HOST,required"`
}

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

	ips, err := net.DefaultResolver.LookupIPAddr(ctx, config.LooupHost)
	if err != nil {
		return logger.WrapError(err)
	}

	var (
		v4s []string
		v6s []string
	)

	for _, a := range ips {
		if a.IP.To4() != nil {
			v4s = append(v4s, a.IP.String())
		} else if a.IP.To16() != nil {
			v6s = append(v6s, a.IP.String())
		}
	}

	// Logging both for debugging purposes.
	logger.SetAttrs("lookup_found_ips_v4", v4s, "lookup_found_ips_v6", v6s)

	// We only support IPv4 for target groups
	registerIPs := v4s

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

	var deregisterTargets []types.TargetDescription

	for _, th := range targetHealth.TargetHealthDescriptions {
		if th.Target.Id == nil {
			continue
		}

		// Already added to target group. Don't need to register again.
		registerIPs = slices.DeleteFunc(registerIPs, func(id string) bool {
			return id == *th.Target.Id
		})

		if !slices.Contains(registerIPs, *th.Target.Id) {
			deregisterTargets = append(deregisterTargets, *th.Target)
		}
	}

	logger.SetAttrs("register_targets", registerIPs)
	logger.SetAttrs("deregister_targets", targetDescriptionToSlice(deregisterTargets))

	if len(deregisterTargets) > 0 {
		_, err := elb.DeregisterTargets(ctx, &elasticloadbalancingv2.DeregisterTargetsInput{
			TargetGroupArn: aws.String(config.TargetGroupARN),
			Targets:        deregisterTargets,
		})
		if err != nil {
			return logger.WrapError(err)
		}
	}

	if len(registerIPs) > 0 {
		var targets []types.TargetDescription
		for _, ip := range registerIPs {
			targets = append(targets, types.TargetDescription{
				Id:   aws.String(ip),
				Port: aws.Int32(443),
			})
		}

		_, err := elb.RegisterTargets(ctx, &elasticloadbalancingv2.RegisterTargetsInput{
			TargetGroupArn: aws.String(config.TargetGroupARN),
			Targets:        targets,
		})
		if err != nil {
			return logger.WrapError(err)
		}
	}

	return nil
}

// Returns a slice of target IDs from a slice of TargetDescription
func targetDescriptionToSlice(targets []types.TargetDescription) []string {
	var result []string

	for _, t := range targets {
		if t.Id != nil {
			result = append(result, *t.Id)
		}
	}

	return result
}
