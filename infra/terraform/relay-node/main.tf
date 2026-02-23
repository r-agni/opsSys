################################################################################
# SystemScale Relay Node — AWS c6in.8xlarge per region
#
# One module invocation per region. Called from the root module with:
#   module "relay_us_east" {
#     source     = "./relay-node"
#     region     = "us-east-1"
#     az_count   = 3
#     anycast_ip = var.anycast_ip_us_east
#   }
#
# Instance choice rationale:
#   c6in.8xlarge: 32 vCPU, 64 GB RAM, 50 Gbps ENA Express, local NVMe
#   - ENA Express: sub-100μs latency between same-region instances
#   - 50 Gbps: handles 1000 vehicles × 50Hz × 500KB/s = 250 Gbps peak... with
#     compression this lands at ~5-10 Gbps sustained, well within 50 Gbps
#   - Local NVMe: for SRT video ring buffer (no EBS I/O latency on hot path)
#   - c-family: CPU-optimized (QUIC crypto, AES-NI, packet processing)
#
# 3 instances per region with ECMP Anycast IP → no SPOF, balanced load
################################################################################

terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 4.0"
    }
  }
}

variable "region"       { type = string }
variable "region_id"    { type = string } # short ID, e.g. "us-east"
variable "az_count"     { type = number; default = 3 }
variable "instance_count" { type = number; default = 3 }
variable "vpc_id"       { type = string }
variable "subnet_ids"   { type = list(string) }
variable "nats_cluster_ips" { type = list(string) } # NATS cluster IPs for VPN peering
variable "anycast_ip"   { type = string }
variable "ssh_key_name" { type = string }
variable "relay_ami"    { type = string } # pre-built AMI with relay binaries + systemd units

locals {
  name_prefix = "systemscale-relay-${var.region_id}"
}

# ──────────────────────────────────────────────────────────────────────────────
# Security groups
# ──────────────────────────────────────────────────────────────────────────────

resource "aws_security_group" "relay" {
  name        = "${local.name_prefix}-sg"
  description = "SystemScale relay node security group"
  vpc_id      = var.vpc_id

  # QUIC from vehicles (UDP 443 inbound) — the only public-facing port
  ingress {
    description = "QUIC from vehicles"
    from_port   = 443
    to_port     = 443
    protocol    = "udp"
    cidr_blocks = ["0.0.0.0/0"]
    ipv6_cidr_blocks = ["::/0"]
  }

  # WebSocket from operators (TCP 443 inbound via Anycast)
  ingress {
    description = "WebSocket from operators"
    from_port   = 8443
    to_port     = 8443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
    ipv6_cidr_blocks = ["::/0"]
  }

  # SRT video from vehicles (UDP 9000 range)
  ingress {
    description = "SRT video from vehicles"
    from_port   = 9000
    to_port     = 9099
    protocol    = "udp"
    cidr_blocks = ["0.0.0.0/0"]
    ipv6_cidr_blocks = ["::/0"]
  }

  # Prometheus scrape (internal VPC only)
  ingress {
    description = "Prometheus metrics scrape"
    from_port   = 9090
    to_port     = 9095
    protocol    = "tcp"
    cidr_blocks = [data.aws_vpc.selected.cidr_block]
  }

  # SSH (management, restricted to Tailscale/VPN CIDR in production)
  ingress {
    description = "SSH management"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = ["100.64.0.0/10"] # Tailscale CGNAT range
  }

  # All outbound allowed (relay connects to NATS cluster, LiveKit, Cloudflare Argo)
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name      = "${local.name_prefix}-sg"
    Component = "relay"
    Region    = var.region_id
  }
}

data "aws_vpc" "selected" {
  id = var.vpc_id
}

# ──────────────────────────────────────────────────────────────────────────────
# IAM instance profile
# ──────────────────────────────────────────────────────────────────────────────

resource "aws_iam_role" "relay" {
  name = "${local.name_prefix}-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action    = "sts:AssumeRole"
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
    }]
  })
}

resource "aws_iam_role_policy" "relay" {
  name = "${local.name_prefix}-policy"
  role = aws_iam_role.relay.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        # SSM Session Manager (no SSH bastion needed)
        Effect = "Allow"
        Action = [
          "ssm:UpdateInstanceInformation",
          "ssmmessages:CreateControlChannel",
          "ssmmessages:CreateDataChannel",
          "ssmmessages:OpenControlChannel",
          "ssmmessages:OpenDataChannel"
        ]
        Resource = "*"
      },
      {
        # CloudWatch metrics for relay health monitoring
        Effect   = "Allow"
        Action   = ["cloudwatch:PutMetricData", "cloudwatch:GetMetricData"]
        Resource = "*"
      },
      {
        # S3 read access to SRT ring buffer backups
        Effect   = "Allow"
        Action   = ["s3:PutObject", "s3:GetObject"]
        Resource = "arn:aws:s3:::systemscale-srt-buffer/${var.region_id}/*"
      }
    ]
  })
}

resource "aws_iam_instance_profile" "relay" {
  name = "${local.name_prefix}-profile"
  role = aws_iam_role.relay.name
}

# ──────────────────────────────────────────────────────────────────────────────
# Launch template — c6in.8xlarge with NVMe and ENA Express
# ──────────────────────────────────────────────────────────────────────────────

resource "aws_launch_template" "relay" {
  name          = "${local.name_prefix}-lt"
  image_id      = var.relay_ami
  instance_type = "c6in.8xlarge"
  key_name      = var.ssh_key_name

  iam_instance_profile {
    name = aws_iam_instance_profile.relay.name
  }

  network_interfaces {
    security_groups             = [aws_security_group.relay.id]
    associate_public_ip_address = true
    # ENA Express: enables sub-100μs latency between same-region instances
    # Required for NATS leaf node co-location latency targets
    ena_srd_enabled             = true
    ena_srd_udp_enabled         = true # Critical: ENA Express for UDP (QUIC transport)
    delete_on_termination       = true
  }

  # NVMe ephemeral storage — for SRT video ring buffer (60s, ~4GB at 8Mbps H.265)
  # Instance store survives reboots but not stop/start (acceptable for ring buffer)
  block_device_mappings {
    device_name = "/dev/xvda"
    ebs {
      volume_size           = 50   # GB — root volume for OS + relay binaries
      volume_type           = "gp3"
      iops                  = 3000
      throughput            = 125
      delete_on_termination = true
      encrypted             = true
    }
  }

  # Instance store (NVMe, ephemeral) is configured via Ansible after launch
  # c6in.8xlarge provides 1 × 1,900 GB NVMe SSD (instance store, mapped as /dev/nvme1n1)

  # User data: minimal bootstrap (Ansible does the real config)
  user_data = base64encode(<<-EOF
    #!/bin/bash
    set -e
    # Signal Ansible/SSM that the instance is ready
    /usr/bin/aws ssm put-parameter \
      --name "/systemscale/relay/${var.region_id}/$(curl -s http://169.254.169.254/latest/meta-data/instance-id)/ready" \
      --value "$(date -Iseconds)" \
      --type String \
      --overwrite \
      --region ${var.region} || true
    # Relay services are managed by systemd units (installed via Ansible / AMI bake)
    systemctl enable --now systemscale-relay
    systemctl enable --now systemscale-ws-gateway
    systemctl enable --now systemscale-command-forwarder
    systemctl enable --now systemscale-srt-relay
    systemctl enable --now nats-leaf
  EOF
  )

  tag_specifications {
    resource_type = "instance"
    tags = {
      Name      = "${local.name_prefix}"
      Component = "relay"
      Region    = var.region_id
    }
  }

  metadata_options {
    http_tokens                 = "required" # IMDSv2 only
    http_put_response_hop_limit = 1
  }
}

# ──────────────────────────────────────────────────────────────────────────────
# Auto Scaling Group — 3 instances per region (one per AZ)
# ──────────────────────────────────────────────────────────────────────────────

resource "aws_autoscaling_group" "relay" {
  name                = "${local.name_prefix}-asg"
  min_size            = var.instance_count
  max_size            = var.instance_count * 2 # allow doubling for planned events
  desired_capacity    = var.instance_count
  vpc_zone_identifier = var.subnet_ids

  launch_template {
    id      = aws_launch_template.relay.id
    version = "$Latest"
  }

  health_check_type         = "EC2"
  health_check_grace_period = 120

  # Instance refresh: rolling update with 33% healthy at all times
  instance_refresh {
    strategy = "Rolling"
    preferences {
      min_healthy_percentage = 67
    }
  }

  tag {
    key                 = "Name"
    value               = local.name_prefix
    propagate_at_launch = true
  }
  tag {
    key                 = "Component"
    value               = "relay"
    propagate_at_launch = true
  }
}

# ──────────────────────────────────────────────────────────────────────────────
# AWS Global Accelerator endpoint
# (traffic from vehicles is also sent via Cloudflare Argo — active-active)
# ──────────────────────────────────────────────────────────────────────────────

resource "aws_globalaccelerator_accelerator" "relay" {
  count   = var.region == "us-east-1" ? 1 : 0 # only create once for global resource
  name    = "systemscale-relay"
  enabled = true
  ip_address_type = "DUAL_STACK"

  attributes {
    flow_logs_enabled   = true
    flow_logs_s3_bucket = "systemscale-logs"
    flow_logs_s3_prefix = "globalaccelerator/"
  }
}

# ──────────────────────────────────────────────────────────────────────────────
# Outputs
# ──────────────────────────────────────────────────────────────────────────────

output "asg_name" {
  value = aws_autoscaling_group.relay.name
}

output "security_group_id" {
  value = aws_security_group.relay.id
}

output "launch_template_id" {
  value = aws_launch_template.relay.id
}
