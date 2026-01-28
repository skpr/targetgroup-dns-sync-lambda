Lambda: ALB TargetGroup DNS Sync
================================

A Lambda for syncing IP addresses from a DNS records to a ALB Target Group.

## Configuration

```
STREAM_NAME=<unique identifier for logging>
LOOKUP_HOST=<dns for the host to lookup and sync>
TARGET_GROUP_ARN=<ARN of the target group>
```
