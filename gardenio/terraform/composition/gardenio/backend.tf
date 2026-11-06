terraform {
  backend "s3" {
    bucket         = "gardenio-tfstate-360190250083"
    key            = "composition/gardenio/terraform.tfstate"
    region         = "us-east-1"
    dynamodb_table = "gardenio-tflock"
    encrypt        = true
    profile        = "gardenio"
  }
}
