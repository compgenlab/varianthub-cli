package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/compgenlab/varianthub-cli/internal/config"
)

// cmdEdit launches the interactive snapshot editor (BubbleTea TUI): a master-detail
// forms UI to browse and build snapshots → sources/tools → annotations, with
// required/missing-field indicators. An optional arg opens a snapshot directly.
func cmdEdit(cfgPath string, args []string) error {
	if err := config.MustExist(cfgPath); err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	m := &editModel{cfg: cfg, cfgPath: cfgPath, width: 80, height: 24}
	if len(args) > 0 {
		m.toFragments(args[0])
	} else {
		m.toHome()
	}
	_, err = tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

// --- styles -----------------------------------------------------------------

var (
	cAccent  = lipgloss.AdaptiveColor{Light: "26", Dark: "39"}   // blue
	cAccent2 = lipgloss.AdaptiveColor{Light: "30", Dark: "79"}   // teal
	cSubtle  = lipgloss.AdaptiveColor{Light: "246", Dark: "244"} // gray
	cLight   = lipgloss.Color("231")
	cRed     = lipgloss.Color("203")

	okStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	errStyle  = lipgloss.NewStyle().Foreground(cRed).Bold(true)

	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(cLight).Background(cAccent).Padding(0, 1)
	footerStyle = lipgloss.NewStyle().Padding(0, 1)
	keyStyle    = lipgloss.NewStyle().Foreground(cAccent2).Bold(true)
	descStyle   = lipgloss.NewStyle().Foreground(cSubtle)
	errPill     = lipgloss.NewStyle().Foreground(cLight).Background(cRed).Bold(true).Padding(0, 1)
)

// formTheme is the shared huh theme — base structure recolored to the accent palette.
func formTheme() *huh.Theme {
	t := huh.ThemeBase()
	t.Focused.Base = t.Focused.Base.BorderForeground(cAccent)
	t.Focused.Title = t.Focused.Title.Foreground(cAccent).Bold(true)
	t.Focused.Description = t.Focused.Description.Foreground(cSubtle)
	t.Focused.SelectSelector = t.Focused.SelectSelector.Foreground(cAccent)
	t.Focused.SelectedOption = t.Focused.SelectedOption.Foreground(cAccent2).Bold(true)
	t.Focused.FocusedButton = t.Focused.FocusedButton.Background(cAccent).Foreground(cLight)
	t.Focused.ErrorIndicator = t.Focused.ErrorIndicator.Foreground(cRed)
	t.Blurred.Title = t.Blurred.Title.Foreground(cSubtle)
	t.Blurred.Base = t.Blurred.Base.BorderForeground(cSubtle)
	return t
}

// styledDelegate is the list item renderer with an accent-highlighted selection.
func styledDelegate() list.DefaultDelegate {
	d := list.NewDefaultDelegate()
	d.Styles.SelectedTitle = d.Styles.SelectedTitle.Foreground(cAccent).BorderForeground(cAccent).Bold(true)
	d.Styles.SelectedDesc = d.Styles.SelectedDesc.Foreground(cAccent2).BorderForeground(cAccent)
	d.Styles.NormalDesc = d.Styles.NormalDesc.Foreground(cSubtle)
	return d
}

// --- model ------------------------------------------------------------------

type screen int

const (
	scrHome screen = iota
	scrSources
	scrConfig
	scrSnapshots
	scrFragments
	scrSourceMenu
	scrSourceForm
	scrAnnotations
	scrAnnForm
	scrSourceDefaults
	scrBuiltins
	scrConfirm
	scrNewSnap
	scrBuiltinArgs
	scrSnapMembers
	scrSnapDefaults
)

type editModel struct {
	cfg     *config.Config
	cfgPath string

	screen   screen
	list     list.Model
	form     *huh.Form
	width    int
	height   int
	err      error
	quitting bool

	// working state
	curSnap     string
	curPath     string // fragment path of the source being edited ("" = new)
	curSource   *config.Source
	annIdx      int
	libraryMode bool // editing a source in the top-level library (not within a snapshot)
	annFromForm bool // annotations editor entered from the source form (vs the source menu)

	// config.toml editor scratch (raw config; $VARHUB_HOME literals preserved)
	cfgEdit       *config.Config
	cfgRegistries string
	cfgRefAsm     string // the default snapshot's assembly, whose FASTA we expose
	cfgRefFasta   string

	// form-bound scratch
	newSnapName    string
	refColStr      string
	altColStr      string
	action         string
	annWork        config.Annotation
	annMatch       string
	confirmVal     bool
	builtinFrag    *config.Snapshot
	builtinPath    string
	pendingBuiltin string
	argsVal        string

	// snapshot manifest editors (checkbox multi-selects)
	memberSources []string
	defaultAnns   []string
}

func (m *editModel) Init() tea.Cmd { return nil }

func (m *editModel) isForm() bool {
	switch m.screen {
	case scrSourceForm, scrAnnForm, scrConfirm, scrNewSnap, scrBuiltinArgs,
		scrSnapMembers, scrSnapDefaults, scrSourceDefaults, scrConfig:
		return true
	}
	return false
}

// item is a generic list row; kind/payload/idx drive navigation.
type item struct {
	title, desc, kind, payload string
	idx                        int
}

func (i item) Title() string       { return i.title }
func (i item) Description() string { return i.desc }
func (i item) FilterValue() string { return i.title }

// setList builds the active list (title/help are rendered by our header/footer).
func (m *editModel) setList(items []list.Item) {
	l := list.New(items, styledDelegate(), m.width, m.height)
	l.SetShowTitle(false)
	l.SetShowHelp(false)
	l.Styles.StatusBar = l.Styles.StatusBar.Foreground(cSubtle)
	l.Styles.StatusEmpty = l.Styles.StatusEmpty.Foreground(cSubtle)
	m.list = l
	m.sizeList()
}

// contentH is the rows available between the header and footer bars.
func (m *editModel) contentH() int { return maxi(3, m.height-3) }

func (m *editModel) sizeList() { m.list.SetSize(maxi(20, m.width), m.contentH()) }
func (m *editModel) sizeForm() {
	if m.form != nil {
		m.form = m.form.WithWidth(maxi(40, m.width-2)).WithHeight(m.contentH())
	}
}

// --- update -----------------------------------------------------------------

func (m *editModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.isForm() {
			m.sizeForm()
		} else {
			m.sizeList()
		}
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.quitting = true
			return m, tea.Quit
		}
		m.err = nil
		if m.isForm() {
			// ctrl+g is a reliable cancel/back that doesn't depend on ESC (which tmux
			// delays or swallows) and never collides with text input.
			if msg.String() == "ctrl+g" {
				return m, m.onFormAbort()
			}
			return m.updateForm(msg)
		}
		return m.updateList(msg)
	}
	// non-key messages → active component
	if m.isForm() {
		return m.updateForm(msg)
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m *editModel) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	sel, _ := m.list.SelectedItem().(item)
	switch m.screen {
	case scrHome:
		switch msg.String() {
		case "q", "esc":
			m.quitting = true
			return m, tea.Quit
		case "enter":
			switch sel.kind {
			case "config":
				return m, m.toConfig()
			case "sources":
				return m, m.toSources()
			case "snapshots":
				return m, m.toSnapshots()
			}
		}
	case scrSources:
		switch msg.String() {
		case "esc", "q":
			return m, m.toHome()
		case "s":
			m.startNewSource()
			return m, m.toSourceForm()
		case "b":
			return m, m.toBuiltins("")
		case "enter":
			switch sel.kind {
			case "source":
				m.openSource(sel.payload)
				return m, m.toSourceMenu()
			case "builtin", "addbuiltin":
				return m, m.toBuiltins("")
			case "addsource":
				m.startNewSource()
				return m, m.toSourceForm()
			}
		}
	case scrSnapshots:
		switch msg.String() {
		case "q", "esc":
			return m, m.toHome()
		case "n":
			return m, m.toNewSnap()
		case "enter":
			if sel.kind == "newsnap" {
				return m, m.toNewSnap()
			}
			if sel.kind == "snapshot" {
				return m, m.toFragments(sel.payload)
			}
		}
	case scrFragments:
		// A snapshot only *selects* from already-configured sources; new sources are
		// created in the Sources library, not here.
		switch msg.String() {
		case "esc", "q":
			return m, m.toSnapshots()
		case "m":
			return m, m.toSnapMembers()
		case "d":
			return m, m.toSnapDefaults()
		case "enter":
			switch sel.kind {
			case "source": // open an already-included source (edit / select defaults)
				m.openSource(sel.payload)
				return m, m.toSourceMenu()
			case "builtin":
				return m, m.toBuiltins(m.curSnap)
			}
		}
	case scrSourceMenu:
		switch msg.String() {
		case "esc", "q":
			return m, m.srcReturn()
		case "enter":
			switch sel.kind {
			case "editsrc":
				return m, m.toSourceForm()
			case "editann":
				m.annFromForm = false
				return m, m.toAnnotations()
			case "seldef":
				return m, m.toSourceDefaults()
			}
		}
	case scrAnnotations:
		switch msg.String() {
		case "esc", "q":
			if m.annFromForm {
				return m, m.toSourceForm()
			}
			return m, m.toSourceMenu()
		case "d":
			if sel.kind == "ann" {
				m.curSource.Annotations = remove(m.curSource.Annotations, sel.idx)
				m.persistCurSource()
				return m, m.toAnnotations()
			}
		case "enter":
			if sel.kind == "addann" {
				return m, m.toAnnForm(-1)
			}
			if sel.kind == "ann" {
				return m, m.toAnnForm(sel.idx)
			}
		}
	case scrBuiltins:
		switch msg.String() {
		case "esc", "q":
			return m, m.srcReturn()
		case "d":
			if sel.kind == "present" {
				m.removeBuiltin(sel.payload)
				if err := m.saveBuiltins(); err != nil {
					m.err = err
				}
				return m, m.toBuiltins(m.curSnap)
			}
		case "enter":
			if sel.kind == "builtinadd" {
				if sel.payload == "tags" || sel.payload == "copy_logratio" {
					return m, m.toBuiltinArgs(sel.payload)
				}
				m.appendBuiltin(sel.payload, "")
				if err := m.saveBuiltins(); err != nil {
					m.err = err
				}
				return m, m.toBuiltins(m.curSnap)
			}
		}
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m *editModel) updateForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	f, cmd := m.form.Update(msg)
	if ff, ok := f.(*huh.Form); ok {
		m.form = ff
	}
	switch m.form.State {
	case huh.StateCompleted:
		return m, m.onFormComplete()
	case huh.StateAborted:
		return m, m.onFormAbort()
	}
	return m, cmd
}

func (m *editModel) onFormComplete() tea.Cmd {
	switch m.screen {
	case scrNewSnap:
		if m.newSnapName == "" {
			return m.toSnapshots()
		}
		if err := config.WriteSnapshotConfig(m.cfg.SnapshotFile(m.newSnapName), &config.SnapshotConfig{}); err != nil {
			m.err = err
			return m.toSnapshots()
		}
		return m.toFragments(m.newSnapName)
	case scrSourceForm:
		switch m.action {
		case "annotations":
			m.annFromForm = true
			return m.toAnnotations()
		case "delete":
			return m.toConfirm(m.curPath)
		case "cancel":
			return m.srcReturn()
		default: // save
			if err := m.saveSource(); err != nil {
				m.err = err
				return m.toSourceForm()
			}
			return m.srcReturn()
		}
	case scrAnnForm:
		switch m.action {
		case "delete":
			if m.annIdx >= 0 {
				m.curSource.Annotations = remove(m.curSource.Annotations, m.annIdx)
				m.persistCurSource()
			}
			return m.toAnnotations()
		case "cancel":
			return m.toAnnotations()
		default: // save
			m.annWork.Match = ""
			if m.annMatch == "position" {
				m.annWork.Match = "position"
			}
			if m.annWork.Name == "" {
				m.err = fmt.Errorf("annotation needs a name")
				return m.toAnnForm(m.annIdx)
			}
			if m.annIdx >= 0 {
				m.curSource.Annotations[m.annIdx] = m.annWork
			} else {
				m.curSource.Annotations = append(m.curSource.Annotations, m.annWork)
			}
			m.persistCurSource()
			return m.toAnnotations()
		}
	case scrConfirm:
		if m.confirmVal {
			os.Remove(m.curPath)
		}
		return m.srcReturn()
	case scrConfig:
		if m.action != "cancel" {
			if err := m.saveConfig(); err != nil {
				m.err = err
				return m.toConfig()
			}
		}
		return m.toHome()
	case scrBuiltinArgs:
		if m.argsVal != "" {
			m.appendBuiltin(m.pendingBuiltin, m.argsVal)
			if err := m.saveBuiltins(); err != nil {
				m.err = err
			}
		}
		return m.toBuiltins(m.curSnap)
	case scrSnapMembers:
		if err := m.saveSnapMembers(); err != nil {
			m.err = err
		}
		return m.toFragments(m.curSnap)
	case scrSnapDefaults:
		if err := m.saveSnapDefaults(); err != nil {
			m.err = err
		}
		return m.toFragments(m.curSnap)
	case scrSourceDefaults:
		if err := m.saveSourceDefaults(); err != nil {
			m.err = err
		}
		return m.toSourceMenu()
	}
	return nil
}

func (m *editModel) onFormAbort() tea.Cmd {
	switch m.screen {
	case scrSourceForm:
		return m.srcReturn()
	case scrAnnForm:
		return m.toAnnotations()
	case scrSourceDefaults:
		return m.toSourceMenu()
	case scrConfig:
		return m.toHome()
	case scrNewSnap:
		return m.toSnapshots()
	case scrConfirm:
		return m.srcReturn()
	case scrBuiltinArgs:
		return m.toBuiltins(m.curSnap)
	case scrSnapMembers, scrSnapDefaults:
		return m.toFragments(m.curSnap)
	}
	return nil
}

// --- screens ----------------------------------------------------------------

// toHome is the top-level menu: config settings, the source library, and snapshots.
func (m *editModel) toHome() tea.Cmd {
	m.screen = scrHome
	m.libraryMode = false
	m.setList([]list.Item{
		item{title: "Config settings", desc: "edit config.toml (dirs, database, registries, reference)", kind: "config"},
		item{title: "Sources", desc: "browse, add, and edit the local source library", kind: "sources"},
		item{title: "Snapshots", desc: "add sources to snapshots + choose default annotations", kind: "snapshots"},
	})
	return nil
}

// toSources browses the whole local source library (every source on disk, regardless of
// which snapshot references it). Editing here does not touch any snapshot.
func (m *editModel) toSources() tea.Cmd {
	m.screen = scrSources
	m.libraryMode = true
	m.curSnap = ""
	refs, _ := m.cfg.ListSources()
	var items []list.Item
	for _, ref := range refs {
		n, v, err := m.cfg.ResolveSourceRef(ref)
		if err != nil {
			continue
		}
		f := m.cfg.SourceFile(n, v)
		frag, err := config.ReadFragment(f)
		if err != nil || len(frag.Sources) == 0 {
			items = append(items, item{title: ref, desc: errStyle.Render("parse error"), kind: "bad"})
			continue
		}
		s := frag.Sources[0]
		switch {
		case s.IsBuiltinSource():
			items = append(items, item{title: ref + "  ⟨builtin⟩",
				desc: fmt.Sprintf("%d builtin annotation(s)", len(s.Annotations)), kind: "builtin", payload: f})
		case s.IsTool():
			items = append(items, item{title: ref + "  ⟨tool⟩ (" + orDash(s.Format) + ")",
				desc: styledBadge(missingToolFields(s.AsTool())) + annCount(len(s.Annotations)), kind: "source", payload: f})
		default:
			items = append(items, item{title: ref + "  (" + orDash(s.Format) + ")",
				desc: styledBadge(missingSourceFields(s)) + annCount(len(s.Annotations)), kind: "source", payload: f})
		}
	}
	items = append(items, item{title: "＋ Add source", kind: "addsource"})
	items = append(items, item{title: "＋ Builtins", kind: "addbuiltin"})
	m.setList(items)
	return nil
}

// toConfig edits config.toml. It uses ReadConfigFile so $VARHUB_HOME literals round-trip
// instead of being baked into absolute paths.
func (m *editModel) toConfig() tea.Cmd {
	m.screen = scrConfig
	raw, err := config.ReadConfigFile(m.cfgPath)
	if err != nil {
		m.err = err
		return m.toHome()
	}
	if raw.Database.Backend == "" {
		raw.Database.Backend = "none" // "" = cache disabled; show it explicitly
	}
	m.cfgEdit = raw
	m.cfgRegistries = strings.Join(raw.Registries, "\n")
	// Expose the reference FASTA for the default snapshot's assembly (a convenience; the
	// full per-assembly [references.<assembly>] map is edited in the file).
	m.cfgRefAsm, m.cfgRefFasta = "", ""
	if sc, e := config.ReadSnapshotConfig(m.cfg.SnapshotFile(raw.DefaultSnapshot)); e == nil && sc.Assembly != "" {
		m.cfgRefAsm = sc.Assembly
		m.cfgRefFasta = raw.References[sc.Assembly].Fasta
	}
	m.action = "save"

	fields := []huh.Field{
		huh.NewInput().Title("data_dir").Value(&m.cfgEdit.DataDir),
		huh.NewInput().Title("cache_dir").Value(&m.cfgEdit.CacheDir),
		huh.NewInput().Title("annotations_dir").Value(&m.cfgEdit.AnnotationsDir),
	}
	if snaps, _ := m.cfg.ListSnapshots(); len(snaps) > 0 {
		fields = append(fields, huh.NewSelect[string]().Title("default_snapshot").
			Options(huh.NewOptions(snaps...)...).Value(&m.cfgEdit.DefaultSnapshot))
	} else {
		fields = append(fields, huh.NewInput().Title("default_snapshot").Value(&m.cfgEdit.DefaultSnapshot))
	}
	fields = append(fields,
		huh.NewSelect[string]().Title("database backend").Description("none = cache disabled").
			Options(huh.NewOptions("sqlite", "postgres", "none")...).Value(&m.cfgEdit.Database.Backend),
		huh.NewInput().Title("database path/DSN").Value(&m.cfgEdit.Database.Path),
		huh.NewText().Title("registries").Description("one registry.toml URL per line").Value(&m.cfgRegistries),
	)
	if m.cfgRefAsm != "" {
		fields = append(fields, huh.NewInput().Title("reference FASTA ("+m.cfgRefAsm+")").
			Description("[references."+m.cfgRefAsm+"]; other assemblies are edited in the file").
			Value(&m.cfgRefFasta))
	}
	fields = append(fields, huh.NewSelect[string]().Title("▸ action").
		Options(huh.NewOption("Save", "save"), huh.NewOption("Cancel", "cancel")).Value(&m.action))

	m.form = huh.NewForm(huh.NewGroup(fields...)).WithTheme(formTheme()).WithShowHelp(true)
	m.sizeForm()
	return m.form.Init()
}

func (m *editModel) saveConfig() error {
	c := m.cfgEdit
	var regs []string
	for _, line := range strings.Split(m.cfgRegistries, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			regs = append(regs, s)
		}
	}
	c.Registries = regs
	if m.cfgRefAsm != "" {
		if c.References == nil {
			c.References = map[string]config.Reference{}
		}
		if strings.TrimSpace(m.cfgRefFasta) == "" {
			delete(c.References, m.cfgRefAsm)
		} else {
			c.References[m.cfgRefAsm] = config.Reference{Fasta: m.cfgRefFasta}
		}
	}
	if err := config.WriteTOML(m.cfgPath, *c); err != nil {
		return err
	}
	if reloaded, err := config.Load(m.cfgPath); err == nil {
		m.cfg = reloaded // let the rest of the TUI see the change
	}
	return nil
}

func (m *editModel) toSnapshots() tea.Cmd {
	m.screen = scrSnapshots
	m.libraryMode = false
	names, _ := m.cfg.ListSnapshots()
	var items []list.Item
	for _, n := range names {
		d := ""
		if n == m.cfg.DefaultSnapshot {
			d = "default"
		}
		items = append(items, item{title: n, desc: d, kind: "snapshot", payload: n})
	}
	items = append(items, item{title: "＋ New snapshot", desc: "create an empty snapshot", kind: "newsnap"})
	m.setList(items)
	return nil
}

// srcReturn is where the shared source/builtin editors go back to: the library browser
// in library mode, else the current snapshot's fragment list.
func (m *editModel) srcReturn() tea.Cmd {
	if m.libraryMode {
		return m.toSources()
	}
	return m.toFragments(m.curSnap)
}

func (m *editModel) toFragments(snap string) tea.Cmd {
	m.screen = scrFragments
	m.libraryMode = false
	m.curSnap = snap
	var items []list.Item
	files := m.manifestItemFiles(snap)
	for _, f := range files {
		frag, err := config.ReadFragment(f)
		base := filepath.Base(f)
		if err != nil {
			items = append(items, item{title: base, desc: errStyle.Render("parse error"), kind: "bad"})
			continue
		}
		for i := range frag.Sources {
			s := frag.Sources[i]
			switch {
			case s.IsBuiltinSource():
				items = append(items, item{title: base + "  ⟨builtin⟩",
					desc: fmt.Sprintf("%d builtin annotation(s)", len(s.Annotations)), kind: "builtin", payload: f})
			case s.IsTool():
				items = append(items, item{
					title: fmt.Sprintf("%s  tool %s@%s (%s)", base, s.Name, s.Version, orDash(s.Format)),
					desc:  styledBadge(missingToolFields(s.AsTool())) + annCount(len(s.Annotations)), kind: "source", payload: f,
				})
			default:
				items = append(items, item{
					title: fmt.Sprintf("%s  %s@%s (%s)", base, s.Name, s.Version, orDash(s.Format)),
					desc:  styledBadge(missingSourceFields(s)) + annCount(len(s.Annotations)),
					kind:  "source", payload: f,
				})
			}
		}
	}
	if len(items) == 0 {
		items = append(items, item{title: "(no sources in this snapshot yet)",
			desc: "press m to select from the source library", kind: "empty"})
	}
	m.setList(items)
	return nil
}

// manifestItemFiles resolves a snapshot manifest's source refs (data + tool) to their
// top-level file paths (skipping refs that don't resolve locally).
func (m *editModel) manifestItemFiles(snap string) []string {
	sc, err := config.ReadSnapshotConfig(m.cfg.SnapshotFile(snap))
	if err != nil {
		m.err = err
		return nil
	}
	var files []string
	for _, ref := range sc.Sources {
		if n, v, e := m.cfg.ResolveSourceRef(ref); e == nil {
			files = append(files, m.cfg.SourceFile(n, v))
		}
	}
	return files
}

// toSnapMembers is the checkbox editor for which sources this snapshot references
// (data, builtin, and tool sources alike). Options are every available name:version
// on disk; the currently-referenced ones are pre-checked. Saving rewrites the
// manifest's sources list (pruning any default_annotations that no longer resolve).
func (m *editModel) toSnapMembers() tea.Cmd {
	m.screen = scrSnapMembers
	sc, err := config.ReadSnapshotConfig(m.cfg.SnapshotFile(m.curSnap))
	if err != nil {
		m.err = err
		return m.toFragments(m.curSnap)
	}
	srcRefs, _ := m.cfg.ListSources()
	m.memberSources = append([]string(nil), sc.Sources...)

	if len(srcRefs) == 0 {
		m.err = fmt.Errorf("no sources available — add one first (press s)")
		return m.toFragments(m.curSnap)
	}
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().Title("sources in this snapshot").
			Description("data, builtin, and tool sources").
			Options(selectedOptions(srcRefs, m.memberSources)...).Value(&m.memberSources),
	)).WithTheme(formTheme()).WithShowHelp(true)
	m.sizeForm()
	return m.form.Init()
}

func (m *editModel) saveSnapMembers() error {
	file := m.cfg.SnapshotFile(m.curSnap)
	sc, err := config.ReadSnapshotConfig(file)
	if err != nil {
		return err
	}
	sc.Sources = m.memberSources
	if err := config.WriteSnapshotConfig(file, sc); err != nil {
		return err
	}
	// Drop defaults that no longer resolve to any included annotation. This must
	// run after the write above, since snapshotAnnotationNames re-reads the manifest.
	if len(sc.Defaults) > 0 {
		pruned := filterIn(sc.Defaults, stringSet(m.snapshotAnnotationNames()))
		if len(pruned) != len(sc.Defaults) {
			sc.Defaults = pruned
			return config.WriteSnapshotConfig(file, sc)
		}
	}
	return nil
}

// toSnapDefaults is the checkbox editor for the snapshot's default_annotations —
// the annotations applied when `annotate` runs without --all/-a. Options are every
// annotation name provided by the snapshot's included sources/tools.
func (m *editModel) toSnapDefaults() tea.Cmd {
	m.screen = scrSnapDefaults
	sc, err := config.ReadSnapshotConfig(m.cfg.SnapshotFile(m.curSnap))
	if err != nil {
		m.err = err
		return m.toFragments(m.curSnap)
	}
	names := m.snapshotAnnotationNames()
	if len(names) == 0 {
		m.err = fmt.Errorf("no annotations available — add sources/annotations first")
		return m.toFragments(m.curSnap)
	}
	m.defaultAnns = filterIn(sc.Defaults, stringSet(names))
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().Title("default annotations").
			Description("applied when `annotate` runs without --all/-a").
			Options(selectedOptions(names, m.defaultAnns)...).Value(&m.defaultAnns),
	)).WithTheme(formTheme()).WithShowHelp(true)
	m.sizeForm()
	return m.form.Init()
}

func (m *editModel) saveSnapDefaults() error {
	file := m.cfg.SnapshotFile(m.curSnap)
	sc, err := config.ReadSnapshotConfig(file)
	if err != nil {
		return err
	}
	sc.Defaults = m.defaultAnns
	return config.WriteSnapshotConfig(file, sc)
}

// snapshotAnnotationNames is the union of annotation names provided by the
// snapshot's referenced sources/tools (builtins contribute their builtin name),
// de-duplicated and sorted — the option set for the defaults editor.
func (m *editModel) snapshotAnnotationNames() []string {
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, f := range m.manifestItemFiles(m.curSnap) {
		frag, err := config.ReadFragment(f)
		if err != nil {
			continue
		}
		for _, s := range frag.Sources {
			for _, a := range s.Annotations {
				if s.IsBuiltinSource() {
					add(a.Builtin)
				} else {
					add(a.Name) // data + tool sources
				}
			}
		}
	}
	sort.Strings(names)
	return names
}

func (m *editModel) startNewSource() {
	m.curPath = ""
	// Default a new source's assembly to the snapshot it's being added to (the
	// snapshot owns assembly now); blank if the manifest doesn't set one.
	asm := ""
	if sc, err := config.ReadSnapshotConfig(m.cfg.SnapshotFile(m.curSnap)); err == nil {
		asm = sc.Assembly
	}
	m.curSource = &config.Source{Assembly: asm, Format: "vcf"}
}

func (m *editModel) openSource(path string) {
	m.curPath = path
	frag, err := config.ReadFragment(path)
	if err != nil {
		m.err = err
		m.curSource = &config.Source{Format: "vcf"}
		return
	}
	m.curSource = firstFileSource(frag)
	if m.curSource == nil {
		m.curSource = &config.Source{Format: "vcf"}
	}
}

// persistCurSource writes the working source back to its fragment, but only when it's
// an existing file (curPath set). A brand-new source (curPath == "") is persisted by the
// source form's Save instead, so it can still be cancelled.
func (m *editModel) persistCurSource() {
	if m.curPath == "" || m.curSource == nil {
		return
	}
	if err := config.WriteFragment(m.curPath, &config.Snapshot{Sources: []config.Source{*m.curSource}}); err != nil {
		m.err = err
	}
}

// toSourceMenu is the chooser shown when opening an *existing* source: edit the source
// config, or (in a snapshot) pick which of its annotations are that snapshot's defaults.
func (m *editModel) toSourceMenu() tea.Cmd {
	m.screen = scrSourceMenu
	items := []list.Item{
		item{title: "Edit source", desc: "name, url, format, and annotation fields", kind: "editsrc"},
	}
	if m.libraryMode || m.curSnap == "" {
		items = append(items, item{title: "Edit annotations",
			desc: "add / edit this source's annotation fields", kind: "editann"})
	} else {
		items = append(items, item{title: "Select default annotations",
			desc: "mark this source's annotations as defaults for " + m.curSnap, kind: "seldef"})
	}
	m.setList(items)
	return nil
}

// sourceAnnNames is the annotation names this source publishes (builtins use their
// builtin name; data/tool sources use the annotation name).
func (m *editModel) sourceAnnNames() []string {
	var names []string
	builtin := m.curSource != nil && m.curSource.IsBuiltinSource()
	if m.curSource == nil {
		return names
	}
	for _, a := range m.curSource.Annotations {
		if builtin {
			names = append(names, a.Builtin)
		} else {
			names = append(names, a.Name)
		}
	}
	return names
}

// toSourceDefaults picks which of THIS source's annotations are defaults for the current
// snapshot (a per-source view of the snapshot's default_annotations). Snapshot mode only.
func (m *editModel) toSourceDefaults() tea.Cmd {
	m.screen = scrSourceDefaults
	names := m.sourceAnnNames()
	if len(names) == 0 {
		m.err = fmt.Errorf("this source has no annotations to select")
		return m.toSourceMenu()
	}
	sc, err := config.ReadSnapshotConfig(m.cfg.SnapshotFile(m.curSnap))
	if err != nil {
		m.err = err
		return m.toSourceMenu()
	}
	m.defaultAnns = filterIn(sc.Defaults, stringSet(names)) // this source's currently-default names
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().Title("default annotations for " + srcName(m.curSource)).
			Description("checked = applied by default in snapshot " + m.curSnap).
			Options(selectedOptions(names, m.defaultAnns)...).Value(&m.defaultAnns),
	)).WithTheme(formTheme()).WithShowHelp(true)
	m.sizeForm()
	return m.form.Init()
}

// saveSourceDefaults merges this source's selected defaults into the snapshot manifest,
// leaving other sources' defaults untouched.
func (m *editModel) saveSourceDefaults() error {
	file := m.cfg.SnapshotFile(m.curSnap)
	sc, err := config.ReadSnapshotConfig(file)
	if err != nil {
		return err
	}
	mine := stringSet(m.sourceAnnNames())
	kept := sc.Defaults[:0:0]
	for _, d := range sc.Defaults { // keep defaults that belong to OTHER sources
		if !mine[d] {
			kept = append(kept, d)
		}
	}
	sc.Defaults = append(kept, m.defaultAnns...)
	return config.WriteSnapshotConfig(file, sc)
}

func (m *editModel) toSourceForm() tea.Cmd {
	m.screen = scrSourceForm
	s := m.curSource
	m.refColStr = itoaOrEmpty(s.RefCol)
	m.altColStr = itoaOrEmpty(s.AltCol)
	m.action = "save"

	actions := []huh.Option[string]{
		huh.NewOption("Save", "save"),
		huh.NewOption(fmt.Sprintf("Annotations (%d)…", len(s.Annotations)), "annotations"),
	}
	if m.curPath != "" {
		actions = append(actions, huh.NewOption("Delete fragment", "delete"))
	}
	actions = append(actions, huh.NewOption("Cancel", "cancel"))

	m.form = huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("name").Value(&s.Name),
		huh.NewInput().Title("version").Value(&s.Version),
		huh.NewInput().Title("assembly").Value(&s.Assembly),
		huh.NewSelect[string]().Title("format").Options(huh.NewOptions("vcf", "bed", "tab")...).Value(&s.Format),
		huh.NewInput().Title("url (canonical)").Value(&s.URL),
		huh.NewInput().Title("url_index").Value(&s.URLIndex),
		huh.NewInput().Title("localpath").Value(&s.LocalPath),
		huh.NewInput().Title("localpath_index").Value(&s.LocalPathIndex),
		huh.NewInput().Title("checksum").Value(&s.Checksum),
		huh.NewInput().Title("checksum_index").Value(&s.ChecksumIndex),
		huh.NewInput().Title("ref_col (tab)").Value(&m.refColStr),
		huh.NewInput().Title("alt_col (tab)").Value(&m.altColStr),
		huh.NewSelect[string]().Title("▸ action").Options(actions...).Value(&m.action),
	)).WithTheme(formTheme()).WithShowHelp(true)
	m.sizeForm()
	return m.form.Init()
}

func (m *editModel) saveSource() error {
	s := m.curSource
	if s.Format == "tab" {
		s.RefCol = atoiSafe(m.refColStr)
		s.AltCol = atoiSafe(m.altColStr)
	} else {
		s.RefCol, s.AltCol = 0, 0
	}
	var miss []string
	if s.IsTool() { // a tool source round-trips its image/steps; it needs no url
		miss = missingToolFields(s.AsTool())
	} else {
		miss = missingSourceFields(*s)
	}
	if len(miss) > 0 {
		return fmt.Errorf("missing required field(s): %s", strings.Join(miss, ", "))
	}
	newFile := m.curPath == ""
	path := m.curPath
	if newFile {
		path = m.cfg.SourceFile(s.Name, s.Version)
		m.curPath = path
	}
	if err := config.WriteFragment(path, &config.Snapshot{Sources: []config.Source{*s}}); err != nil {
		return err
	}
	// In snapshot mode a newly-created source is also referenced from the snapshot;
	// in the library it's just written to disk.
	if newFile && !m.libraryMode && m.curSnap != "" {
		return addRefToSnapshot(m.cfg, m.curSnap, s.ID())
	}
	return nil
}

func (m *editModel) toAnnotations() tea.Cmd {
	m.screen = scrAnnotations
	var items []list.Item
	for i := range m.curSource.Annotations {
		a := m.curSource.Annotations[i]
		items = append(items, item{
			title: orDash(a.Name),
			desc:  fmt.Sprintf("field=%s · type=%s%s", orDash(a.Field), orDefaultType(a.Type), defMark(a.Default)),
			kind:  "ann", idx: i,
		})
	}
	items = append(items, item{title: "＋ Add annotation", kind: "addann"})
	m.setList(items)
	return nil
}

func (m *editModel) toAnnForm(idx int) tea.Cmd {
	m.screen = scrAnnForm
	m.annIdx = idx
	if idx >= 0 {
		m.annWork = m.curSource.Annotations[idx]
	} else {
		m.annWork = config.Annotation{Type: "categorical"}
	}
	m.annWork.Type = orDefaultType(m.annWork.Type)
	m.annMatch = orDefaultMatch(m.annWork.Match)
	m.action = "save"
	aw := &m.annWork

	actions := []huh.Option[string]{huh.NewOption("Save", "save")}
	if idx >= 0 {
		actions = append(actions, huh.NewOption("Delete", "delete"))
	}
	actions = append(actions, huh.NewOption("Cancel", "cancel"))

	m.form = huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("name (new tag)").Value(&aw.Name),
		huh.NewInput().Title("field (from source)").Value(&aw.Field),
		huh.NewSelect[string]().Title("type").Options(huh.NewOptions(config.AnnotationTypes...)...).Value(&aw.Type),
		huh.NewSelect[string]().Title("match (vcf)").Options(huh.NewOptions("exact", "position")...).Value(&m.annMatch),
		huh.NewInput().Title("description").Value(&aw.Description),
		huh.NewSelect[string]().Title("▸ action").Options(actions...).Value(&m.action),
	)).WithTheme(formTheme()).WithShowHelp(true)
	m.sizeForm()
	return m.form.Init()
}

// --- builtins ---------------------------------------------------------------

func (m *editModel) toBuiltins(snap string) tea.Cmd {
	m.screen = scrBuiltins
	m.curSnap = snap
	var path string
	var frag *config.Snapshot
	if n, v, e := m.cfg.ResolveSourceRef("builtins"); e == nil {
		path = m.cfg.SourceFile(n, v)
		frag, _ = config.ReadFragment(path)
	}
	if frag == nil || builtinSourceOf(frag) == nil {
		frag = &config.Snapshot{Sources: []config.Source{{Name: "builtins", Version: "1", Type: "builtin"}}}
		path = ""
	}
	m.builtinFrag, m.builtinPath = frag, path
	bs := builtinSourceOf(frag)

	var items []list.Item
	for i := range bs.Annotations {
		a := bs.Annotations[i]
		d := "added"
		if a.Args != "" {
			d = "args=" + a.Args
		}
		items = append(items, item{title: okStyle.Render("✓ ") + a.Builtin, desc: d, kind: "present", payload: a.Builtin})
	}
	for _, name := range config.BuiltinNames {
		if hasBuiltin(bs, name) {
			continue
		}
		items = append(items, item{title: "＋ " + name, desc: "add this builtin", kind: "builtinadd", payload: name})
	}
	m.setList(items)
	return nil
}

func (m *editModel) appendBuiltin(name, args string) {
	if bs := builtinSourceOf(m.builtinFrag); bs != nil {
		bs.Annotations = append(bs.Annotations, config.Annotation{Builtin: name, Args: args})
	}
}

func (m *editModel) removeBuiltin(name string) {
	bs := builtinSourceOf(m.builtinFrag)
	if bs == nil {
		return
	}
	kept := bs.Annotations[:0]
	for _, a := range bs.Annotations {
		if a.Builtin != name {
			kept = append(kept, a)
		}
	}
	bs.Annotations = kept
}

func (m *editModel) saveBuiltins() error {
	bs := builtinSourceOf(m.builtinFrag)
	// An empty builtin fragment is invalid — remove the file instead.
	if bs == nil || len(bs.Annotations) == 0 {
		if m.builtinPath != "" {
			os.Remove(m.builtinPath)
			m.builtinPath = ""
		}
		return nil
	}
	if m.builtinPath == "" {
		m.builtinPath = m.cfg.SourceFile("builtins", "1")
		if !m.libraryMode && m.curSnap != "" {
			if err := addRefToSnapshot(m.cfg, m.curSnap, "builtins:1"); err != nil {
				return err
			}
		}
	}
	return config.WriteFragment(m.builtinPath, m.builtinFrag)
}

func (m *editModel) toBuiltinArgs(name string) tea.Cmd {
	m.screen = scrBuiltinArgs
	m.pendingBuiltin = name
	m.argsVal = ""
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewInput().Title(name + " args (e.g. PANEL:v1)").Value(&m.argsVal),
	)).WithTheme(formTheme())
	m.sizeForm()
	return m.form.Init()
}

// --- new snapshot / delete --------------------------------------------------

func (m *editModel) toNewSnap() tea.Cmd {
	m.screen = scrNewSnap
	m.newSnapName = ""
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("New snapshot name").Value(&m.newSnapName),
	)).WithTheme(formTheme())
	m.sizeForm()
	return m.form.Init()
}

func (m *editModel) toConfirm(path string) tea.Cmd {
	m.screen = scrConfirm
	m.curPath = path
	m.confirmVal = false
	m.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Delete fragment " + filepath.Base(path) + "?").
			Affirmative("Delete").Negative("Cancel").Value(&m.confirmVal),
	)).WithTheme(formTheme())
	m.sizeForm()
	return m.form.Init()
}

// --- view -------------------------------------------------------------------

func (m *editModel) View() string {
	if m.quitting {
		return ""
	}
	header := headerStyle.Width(m.width).Render(m.breadcrumb())
	footer := m.renderFooter()
	var content string
	if m.isForm() {
		content = m.form.View()
	} else {
		content = m.list.View()
	}
	// Pad the content so the footer sticks to the bottom of the alt-screen.
	if gap := m.height - lipgloss.Height(header) - lipgloss.Height(content) - lipgloss.Height(footer); gap > 0 {
		content += strings.Repeat("\n", gap)
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, content, footer)
}

// breadcrumb renders the accent header path (nil-safe per screen).
func (m *editModel) breadcrumb() string {
	parts := []string{"varhub"}
	switch m.screen {
	case scrSources:
		parts = append(parts, "sources")
	case scrConfig:
		parts = append(parts, "config")
	case scrSnapshots:
		parts = append(parts, "snapshots")
	case scrNewSnap:
		parts = append(parts, "snapshots", "new")
	case scrFragments:
		parts = append(parts, m.curSnap)
	case scrSourceMenu, scrSourceForm:
		parts = append(parts, m.ctxLabel(), srcName(m.curSource))
	case scrAnnotations, scrAnnForm:
		parts = append(parts, m.ctxLabel(), srcName(m.curSource), "annotations")
	case scrSourceDefaults:
		parts = append(parts, m.ctxLabel(), srcName(m.curSource), "defaults")
	case scrBuiltins, scrBuiltinArgs:
		parts = append(parts, m.ctxLabel(), "builtins")
	case scrSnapMembers:
		parts = append(parts, m.curSnap, "members")
	case scrSnapDefaults:
		parts = append(parts, m.curSnap, "defaults")
	case scrConfirm:
		parts = append(parts, m.ctxLabel(), "delete?")
	}
	return "❯ " + strings.Join(parts, " ▸ ")
}

// ctxLabel is the breadcrumb segment for the shared source editors: "sources" in library
// mode, else the current snapshot name.
func (m *editModel) ctxLabel() string {
	if m.libraryMode {
		return "sources"
	}
	return m.curSnap
}

func srcName(s *config.Source) string {
	if s == nil || s.Name == "" {
		return "source"
	}
	return s.Name
}

// renderFooter renders the key-hint bar (+ an error pill) along the bottom.
func (m *editModel) renderFooter() string {
	var segs []string
	for _, h := range m.hints() {
		segs = append(segs, keyStyle.Render(h[0])+" "+descStyle.Render(h[1]))
	}
	line := strings.Join(segs, descStyle.Render("  ·  "))
	if m.err != nil {
		line = errPill.Render("⚠ "+m.err.Error()) + "  " + line
	}
	return footerStyle.Width(m.width).Render(line)
}

func (m *editModel) hints() [][2]string {
	switch m.screen {
	case scrHome:
		return [][2]string{{"enter", "open"}, {"q", "quit"}}
	case scrSources:
		return [][2]string{{"enter", "edit"}, {"s", "add source"}, {"b", "builtins"}, {"/", "filter"}, {"q", "home"}}
	case scrSnapshots:
		return [][2]string{{"enter", "open"}, {"n", "new"}, {"/", "filter"}, {"q", "back"}}
	case scrFragments:
		return [][2]string{{"m", "select sources"}, {"d", "defaults"}, {"enter", "edit"}, {"q", "back"}}
	case scrSnapMembers, scrSnapDefaults, scrSourceDefaults:
		return [][2]string{{"space", "toggle"}, {"enter", "save"}, {"^g", "cancel"}}
	case scrSourceMenu:
		return [][2]string{{"enter", "open"}, {"q", "back"}}
	case scrAnnotations:
		return [][2]string{{"enter", "edit"}, {"d", "delete"}, {"q", "back"}}
	case scrBuiltins:
		return [][2]string{{"enter", "add"}, {"d", "remove"}, {"q", "back"}}
	default: // forms
		return [][2]string{{"tab", "next"}, {"enter", "confirm"}, {"^g", "cancel"}}
	}
}

// --- pure helpers (unit-tested; library-agnostic) ---------------------------

// missingSourceFields lists required fields not yet filled on a source.
func missingSourceFields(s config.Source) []string {
	if s.IsBuiltinSource() {
		if len(s.Annotations) == 0 {
			return []string{"annotations"}
		}
		return nil
	}
	var m []string
	if s.Name == "" {
		m = append(m, "name")
	}
	if s.Version == "" {
		m = append(m, "version")
	}
	if s.URL == "" && s.LocalPath == "" && len(s.Files) == 0 {
		m = append(m, "url/localpath")
	}
	return m
}

// missingToolFields lists required fields not yet filled on a tool.
func missingToolFields(t config.Tool) []string {
	var m []string
	if t.Name == "" {
		m = append(m, "name")
	}
	if t.Version == "" {
		m = append(m, "version")
	}
	if len(t.Steps) == 0 {
		m = append(m, "steps")
	}
	return m
}

func badge(missing []string) string {
	if len(missing) == 0 {
		return "✓ complete"
	}
	return "⚠ missing: " + strings.Join(missing, ", ")
}

func styledBadge(missing []string) string {
	if len(missing) == 0 {
		return okStyle.Render(badge(nil))
	}
	return warnStyle.Render(badge(missing))
}

// --- small value helpers ----------------------------------------------------

func atoiSafe(s string) int { n, _ := strconv.Atoi(strings.TrimSpace(s)); return n }

func itoaOrEmpty(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func firstFileSource(frag *config.Snapshot) *config.Source {
	for i := range frag.Sources {
		if !frag.Sources[i].IsBuiltinSource() {
			return &frag.Sources[i]
		}
	}
	return nil
}

func hasBuiltin(s *config.Source, name string) bool {
	for _, a := range s.Annotations {
		if a.Builtin == name {
			return true
		}
	}
	return false
}

func remove[T any](s []T, i int) []T { return append(s[:i], s[i+1:]...) }

// selectedOptions builds huh multi-select options over all, pre-selecting those in
// `on`. huh marks an option selected when NewOption(...).Selected(true) is set.
func selectedOptions(all, on []string) []huh.Option[string] {
	sel := stringSet(on)
	opts := make([]huh.Option[string], 0, len(all))
	for _, v := range all {
		opts = append(opts, huh.NewOption(v, v).Selected(sel[v]))
	}
	return opts
}

func stringSet(s []string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}

// filterIn keeps the elements of s present in keep, preserving order.
func filterIn(s []string, keep map[string]bool) []string {
	var out []string
	for _, v := range s {
		if keep[v] {
			out = append(out, v)
		}
	}
	return out
}

func typeIndex(t string) int { return indexOf(config.AnnotationTypes, orDefaultType(t)) }

func indexOf(opts []string, v string) int {
	for i, o := range opts {
		if o == v {
			return i
		}
	}
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
func orDefaultType(t string) string {
	if t == "" {
		return "categorical"
	}
	return t
}
func orDefaultMatch(m string) string {
	if m == "" {
		return "exact"
	}
	return m
}
func defMark(d bool) string {
	if d {
		return " · default"
	}
	return ""
}
func annCount(n int) string { return fmt.Sprintf("  ·  %d annotation(s)", n) }

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}
