package actionverify

import (
	"context"
	"fmt"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"gopkg.in/yaml.v3"
)

const prowTestContainerName = "test"

func verifyJobEnvironment(ctx context.Context, reader Reader, archive Archive, target models.RemediationTarget) (Result, error) {
	content, ok, err := readSourceFile(ctx, reader, archive, target.Path)
	if err != nil {
		return Result{}, fmt.Errorf("read pinned Prow config %s: %w", target.Path, err)
	}
	if !ok {
		return inconclusive(fmt.Sprintf("Prow config %s was not found", target.Path)), nil
	}
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(content), &document); err != nil {
		return inconclusive(fmt.Sprintf("Prow config %s could not be parsed", target.Path)), nil
	}
	if err := rejectDuplicateYAMLKeys(&document, map[*yaml.Node]bool{}); err != nil {
		return inconclusive(fmt.Sprintf("Prow config %s is ambiguous: %v", target.Path, err)), nil
	}
	jobs := matchingProwJobs(&document, target.Job)
	if len(jobs) != 1 {
		return inconclusive(fmt.Sprintf("Prow job %s matched %d definitions in %s", target.Job, len(jobs), target.Path)), nil
	}
	containers := matchingContainers(jobs[0], target.Container)
	if len(containers) != 1 {
		return inconclusive(fmt.Sprintf("container %s matched %d definitions in Prow job %s", target.Container, len(containers), target.Job)), nil
	}
	env := dereferenceYAML(mappingValue(containers[0], "env"))
	if env == nil {
		return unresolvedJobEnvironment(target, "is not set"), nil
	}
	if env.Kind != yaml.SequenceNode {
		return inconclusive(fmt.Sprintf("env for container %s in Prow job %s is not a sequence", target.Container, target.Job)), nil
	}
	var matches []*yaml.Node
	for _, item := range env.Content {
		item = dereferenceYAML(item)
		if item.Kind == yaml.MappingNode && scalarValue(mappingValue(item, "name")) == target.Name {
			matches = append(matches, item)
		}
	}
	if len(matches) > 1 {
		return inconclusive(fmt.Sprintf("environment variable %s is duplicated in container %s", target.Name, target.Container)), nil
	}
	if len(matches) == 0 {
		return unresolvedJobEnvironment(target, "is not set"), nil
	}
	entry := matches[0]
	if mappingValue(entry, "valueFrom") != nil {
		return inconclusive(fmt.Sprintf("environment variable %s uses valueFrom in container %s", target.Name, target.Container)), nil
	}
	value := dereferenceYAML(mappingValue(entry, "value"))
	if value == nil || value.Kind != yaml.ScalarNode || value.ShortTag() != "!!str" {
		return inconclusive(fmt.Sprintf("environment variable %s has no scalar value in container %s", target.Name, target.Container)), nil
	}
	if scalarExactValue(value) == target.Value {
		return Result{State: StateAlreadyPresent, Reason: fmt.Sprintf("Prow job %s already sets %s=%s in container %s", target.Job, target.Name, target.Value, target.Container)}, nil
	}
	return unresolvedJobEnvironment(target, fmt.Sprintf("is %q", scalarExactValue(value))), nil
}

func unresolvedJobEnvironment(target models.RemediationTarget, current string) Result {
	return Result{State: StateUnresolved, Reason: fmt.Sprintf("Prow job %s container %s environment variable %s %s and requires %q", target.Job, target.Container, target.Name, current, target.Value)}
}

func matchingProwJobs(document *yaml.Node, name string) []*yaml.Node {
	root := dereferenceYAML(document)
	if root != nil && root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = dereferenceYAML(root.Content[0])
	}
	if root == nil || root.Kind != yaml.MappingNode {
		return nil
	}
	var out []*yaml.Node
	appendSequence := func(sequence *yaml.Node) {
		sequence = dereferenceYAML(sequence)
		if sequence == nil || sequence.Kind != yaml.SequenceNode {
			return
		}
		for _, item := range sequence.Content {
			item = dereferenceYAML(item)
			if item.Kind == yaml.MappingNode && scalarValue(mappingValue(item, "name")) == name {
				out = append(out, item)
			}
		}
	}
	appendSequence(mappingValue(root, "periodics"))
	for _, section := range []string{"presubmits", "postsubmits"} {
		repos := dereferenceYAML(mappingValue(root, section))
		if repos == nil || repos.Kind != yaml.MappingNode {
			continue
		}
		for index := 1; index < len(repos.Content); index += 2 {
			appendSequence(repos.Content[index])
		}
	}
	return out
}

func matchingContainers(job *yaml.Node, name string) []*yaml.Node {
	spec := dereferenceYAML(mappingValue(job, "spec"))
	containers := dereferenceYAML(mappingValue(spec, "containers"))
	if containers == nil || containers.Kind != yaml.SequenceNode {
		return nil
	}
	if len(containers.Content) == 1 {
		item := dereferenceYAML(containers.Content[0])
		if item != nil && item.Kind == yaml.MappingNode && name == prowTestContainerName {
			return []*yaml.Node{item}
		}
		return nil
	}
	var out []*yaml.Node
	for _, item := range containers.Content {
		item = dereferenceYAML(item)
		if item.Kind == yaml.MappingNode && scalarValue(mappingValue(item, "name")) == name {
			out = append(out, item)
		}
	}
	return out
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	node = dereferenceYAML(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if scalarValue(node.Content[index]) == key {
			return node.Content[index+1]
		}
	}
	return nil
}
func scalarValue(node *yaml.Node) string {
	node = dereferenceYAML(node)
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}

func scalarExactValue(node *yaml.Node) string {
	node = dereferenceYAML(node)
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}
func dereferenceYAML(node *yaml.Node) *yaml.Node {
	for node != nil && node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	return node
}

func rejectDuplicateYAMLKeys(node *yaml.Node, visiting map[*yaml.Node]bool) error {
	node = dereferenceYAML(node)
	if node == nil || visiting[node] {
		return nil
	}
	visiting[node] = true
	defer delete(visiting, node)
	if node.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := dereferenceYAML(node.Content[index])
			if key == nil || key.Kind != yaml.ScalarNode {
				return fmt.Errorf("mapping contains a non-scalar key")
			}
			identity := key.Tag + "\x00" + key.Value
			if seen[identity] {
				return fmt.Errorf("duplicate key %q", key.Value)
			}
			seen[identity] = true
			if err := rejectDuplicateYAMLKeys(node.Content[index+1], visiting); err != nil {
				return err
			}
		}
		return nil
	}
	for _, child := range node.Content {
		if err := rejectDuplicateYAMLKeys(child, visiting); err != nil {
			return err
		}
	}
	return nil
}
