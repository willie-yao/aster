# Orka base Tools

These files are packaged into the Helm chart's producer ConfigMap. Their source
copies live under `experimental/orka/manifests/`. Run `make helm-check` after
editing either location; the render check fails when the copies drift.
