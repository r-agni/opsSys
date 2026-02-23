################################################################################
# SystemScale Cloud Cluster — EKS + QuestDB + CockroachDB + S3
#
# Deployed in us-east-1 (primary). QuestDB and NATS replicas in eu-west-1 and ap-southeast-1.
################################################################################

terraform {
  required_providers {
    aws = { source = "hashicorp/aws"; version = "~> 5.0" }
  }
}

variable "region"         { default = "us-east-1" }
variable "cluster_name"   { default = "systemscale-cloud" }
variable "vpc_cidr"       { default = "10.10.0.0/16" }
variable "questdb_ami"    { type = string }

locals {
  azs = ["${var.region}a", "${var.region}b", "${var.region}c"]
}

# ──────────────────────────────────────────────────────────────────────────────
# VPC
# ──────────────────────────────────────────────────────────────────────────────

resource "aws_vpc" "main" {
  cidr_block           = var.vpc_cidr
  enable_dns_hostnames = true
  enable_dns_support   = true
  tags = { Name = "${var.cluster_name}-vpc" }
}

resource "aws_subnet" "private" {
  count             = 3
  vpc_id            = aws_vpc.main.id
  cidr_block        = cidrsubnet(var.vpc_cidr, 4, count.index)
  availability_zone = local.azs[count.index]
  tags = {
    Name                              = "${var.cluster_name}-private-${local.azs[count.index]}"
    "kubernetes.io/role/internal-elb" = "1"
  }
}

resource "aws_subnet" "public" {
  count                   = 3
  vpc_id                  = aws_vpc.main.id
  cidr_block              = cidrsubnet(var.vpc_cidr, 4, count.index + 3)
  availability_zone       = local.azs[count.index]
  map_public_ip_on_launch = true
  tags = {
    Name                     = "${var.cluster_name}-public-${local.azs[count.index]}"
    "kubernetes.io/role/elb" = "1"
  }
}

resource "aws_internet_gateway" "igw" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = "${var.cluster_name}-igw" }
}

resource "aws_eip" "nat" {
  count  = 1
  domain = "vpc"
}

resource "aws_nat_gateway" "nat" {
  allocation_id = aws_eip.nat[0].id
  subnet_id     = aws_subnet.public[0].id
  tags          = { Name = "${var.cluster_name}-nat" }
}

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.main.id
  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.nat.id
  }
}

resource "aws_route_table_association" "private" {
  count          = 3
  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private.id
}

# ──────────────────────────────────────────────────────────────────────────────
# EKS Cluster
# ──────────────────────────────────────────────────────────────────────────────

module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "~> 20.0"

  cluster_name    = var.cluster_name
  cluster_version = "1.30"

  vpc_id                         = aws_vpc.main.id
  subnet_ids                     = aws_subnet.private[*].id
  cluster_endpoint_public_access = true

  eks_managed_node_groups = {
    # Kubernetes worker nodes for Go services
    services = {
      instance_types = ["c6i.4xlarge"]  # 16 vCPU, 32 GB, good for IO-bound Go services
      min_size       = 4
      max_size       = 12
      desired_size   = 6
      labels = { role = "services" }
    }
  }

  tags = { Environment = "prod"; Component = "cloud-cluster" }
}

# ──────────────────────────────────────────────────────────────────────────────
# QuestDB — r6i.8xlarge (32 vCPU, 256 GB, io2 SSD)
# Runs as EC2 (not Kubernetes) — StatefulSet on EKS would add I/O overhead
# ──────────────────────────────────────────────────────────────────────────────

resource "aws_instance" "questdb_primary" {
  ami           = var.questdb_ami
  instance_type = "r6i.8xlarge"  # 32 vCPU, 256 GB RAM — QuestDB needs large page cache
  subnet_id     = aws_subnet.private[0].id

  root_block_device {
    volume_type = "io2"
    volume_size = 8000   # 8 TB
    iops        = 64000  # io2 max IOPS for sustained ingestion
    encrypted   = true
  }

  # ENA Express for sub-100μs to EKS nodes (ILP writes from telemetry-ingest)
  # Note: r6i does not support ENA Express SRD — use r6in for ENA Express in production
  # r6i is acceptable here since ILP writes are TCP-based (higher latency tolerance)

  tags = {
    Name      = "${var.cluster_name}-questdb-primary"
    Component = "questdb"
    Role      = "primary"
  }
}

resource "aws_instance" "questdb_replica" {
  count         = 2
  ami           = var.questdb_ami
  instance_type = "r6i.4xlarge"  # 16 vCPU, 128 GB — read replicas (lighter load)
  subnet_id     = aws_subnet.private[count.index + 1].id

  root_block_device {
    volume_type = "gp3"
    volume_size = 4000
    iops        = 16000
    encrypted   = true
  }

  tags = {
    Name      = "${var.cluster_name}-questdb-replica-${count.index}"
    Component = "questdb"
    Role      = "replica"
  }
}

# ──────────────────────────────────────────────────────────────────────────────
# S3 buckets
# ──────────────────────────────────────────────────────────────────────────────

resource "aws_s3_bucket" "recordings" {
  bucket = "systemscale-recordings-${var.region}"
}

resource "aws_s3_bucket_lifecycle_configuration" "recordings" {
  bucket = aws_s3_bucket.recordings.id
  rule {
    id     = "archive-to-glacier"
    status = "Enabled"
    filter { prefix = "recordings/" }

    # Move to Glacier Instant Retrieval after 90 days (10x cost reduction)
    transition {
      days          = 90
      storage_class = "GLACIER_IR"
    }
    # Delete after 2 years (adjust per compliance requirements)
    expiration {
      days = 730
    }
  }
}

resource "aws_s3_bucket" "telemetry_archive" {
  bucket = "systemscale-telemetry-archive-${var.region}"
}

resource "aws_s3_bucket_lifecycle_configuration" "telemetry_archive" {
  bucket = aws_s3_bucket.telemetry_archive.id
  rule {
    id     = "intelligent-tiering"
    status = "Enabled"
    filter { prefix = "telemetry/" }
    transition {
      days          = 30
      storage_class = "INTELLIGENT_TIERING"
    }
  }
}

# ──────────────────────────────────────────────────────────────────────────────
# Outputs
# ──────────────────────────────────────────────────────────────────────────────

output "eks_cluster_endpoint"   { value = module.eks.cluster_endpoint }
output "eks_cluster_name"       { value = module.eks.cluster_name }
output "questdb_primary_ip"     { value = aws_instance.questdb_primary.private_ip }
output "recordings_bucket"      { value = aws_s3_bucket.recordings.bucket }
output "telemetry_archive_bucket" { value = aws_s3_bucket.telemetry_archive.bucket }
output "vpc_id"                 { value = aws_vpc.main.id }
output "private_subnet_ids"     { value = aws_subnet.private[*].id }
