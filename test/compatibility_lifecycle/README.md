This directory contains static analysis scripts for verify functions.

Currently, the following commands are implemented:
```
# Verify feature gate list is up to date
go run test/featuregate_lifecycle/main.go feature-gates verify

# Update feature gate list
go run test/featuregate_lifecycle/main.go feature-gates update
```
