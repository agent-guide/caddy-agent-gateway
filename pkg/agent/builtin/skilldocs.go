package builtin

import (
	"context"
	"fmt"
	"path"

	"github.com/cloudwego/eino/adk/middlewares/skill"

	"github.com/agent-guide/agent-gateway/pkg/agent"
)

// skillDocs serves inline virtual skills to the ADK skill middleware. The
// FrontMatter it returns deliberately carries no Context/Agent/Model fields,
// so fork-mode sub-agent execution and per-skill model overrides are
// structurally unreachable — every skill runs inline, its content returned as
// the skill tool's result.
type skillDocs struct {
	matters []skill.FrontMatter
	byName  map[string]agent.BuiltinSkillDoc
}

func newSkillDocs(skills []agent.BuiltinSkillDoc) *skillDocs {
	b := &skillDocs{byName: make(map[string]agent.BuiltinSkillDoc, len(skills))}
	for _, doc := range skills {
		b.matters = append(b.matters, skill.FrontMatter{Name: doc.Name, Description: doc.Description})
		b.byName[doc.Name] = doc
	}
	return b
}

func (b *skillDocs) List(_ context.Context) ([]skill.FrontMatter, error) {
	return b.matters, nil
}

func (b *skillDocs) Get(_ context.Context, name string) (skill.Skill, error) {
	doc, ok := b.byName[name]
	if !ok {
		return skill.Skill{}, fmt.Errorf("skill %q is not defined for this agent", name)
	}
	return skill.Skill{
		FrontMatter: skill.FrontMatter{Name: doc.Name, Description: doc.Description},
		Content:     doc.Content,
		// The tool result template mentions the skill's directory; there is no
		// filesystem behind inline skills, so give it a stable virtual label.
		BaseDirectory: path.Join("skills", doc.Name),
	}, nil
}
