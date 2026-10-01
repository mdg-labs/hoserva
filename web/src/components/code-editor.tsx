// The one place CodeMirror is imported (Q59): pages use this wrapper and
// never @codemirror/* directly. The editor is styled from coss tokens, so
// the light and dark themes follow the rest of the UI. Tab is left to move
// focus, never bound to indentation, so a keyboard user can always leave.
import { defaultKeymap, history, historyKeymap } from "@codemirror/commands";
import { yaml } from "@codemirror/lang-yaml";
import { HighlightStyle, syntaxHighlighting } from "@codemirror/language";
import { Compartment, EditorState } from "@codemirror/state";
import { drawSelection, EditorView, highlightActiveLine, keymap, lineNumbers } from "@codemirror/view";
import { tags } from "@lezer/highlight";
import { useEffect, useRef } from "react";

import { cn } from "@/lib/utils";

const FRAME =
  "h-[60vh] min-h-72 w-full overflow-hidden rounded-lg border border-input bg-background text-foreground shadow-xs/5 focus-within:border-ring focus-within:ring-[3px] focus-within:ring-ring/24";

const theme = EditorView.theme({
  "&": { height: "100%", color: "var(--foreground)", backgroundColor: "transparent", fontSize: "0.8125rem" },
  "&.cm-focused": { outline: "none" },
  ".cm-scroller": { fontFamily: "var(--font-mono)", lineHeight: "1.6", overflow: "auto" },
  ".cm-content": { caretColor: "var(--foreground)" },
  ".cm-cursor, .cm-dropCursor": { borderLeftColor: "var(--foreground)" },
  ".cm-gutters": { backgroundColor: "transparent", color: "var(--muted-foreground)", border: "none" },
  ".cm-activeLine": { backgroundColor: "var(--code-highlight)" },
  ".cm-activeLineGutter": { backgroundColor: "var(--code-highlight)" },
  ".cm-selectionBackground": { backgroundColor: "color-mix(in srgb, var(--info) 24%, transparent)" },
  "&.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground": {
    backgroundColor: "color-mix(in srgb, var(--info) 32%, transparent)",
  },
});

const highlight = HighlightStyle.define([
  { tag: [tags.lineComment, tags.comment], color: "var(--muted-foreground)", fontStyle: "italic" },
  { tag: [tags.definition(tags.propertyName), tags.definition(tags.string)], color: "var(--info-foreground)" },
  { tag: [tags.string, tags.special(tags.string)], color: "var(--success-foreground)" },
  { tag: [tags.bool, tags.null, tags.number], color: "var(--warning-foreground)" },
  { tag: [tags.labelName, tags.typeName, tags.keyword], color: "var(--warning-foreground)" },
  { tag: [tags.meta, tags.punctuation, tags.squareBracket, tags.brace], color: "var(--muted-foreground)" },
]);

// jsdom has no layout, so CodeMirror cannot measure there and a plain
// textarea stands in. A browser has Range.getClientRects, so it always gets
// the real editor.
function hasLayout(): boolean {
  return typeof Range !== "undefined" && typeof Range.prototype.getClientRects === "function";
}

export function CodeEditor({
  value,
  onChange,
  label,
  readOnly = false,
}: {
  value: string;
  onChange: (value: string) => void;
  label: string;
  readOnly?: boolean;
}): React.ReactElement {
  const native = !hasLayout();
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView | null>(null);
  const readOnlyCompartment = useRef(new Compartment());
  const onChangeRef = useRef(onChange);
  const latest = useRef({ value, readOnly });

  useEffect(() => {
    onChangeRef.current = onChange;
    latest.current = { value, readOnly };
  });

  useEffect(() => {
    if (native || host.current === null) {
      return;
    }
    const editor = new EditorView({
      parent: host.current,
      state: EditorState.create({
        doc: latest.current.value,
        extensions: [
          lineNumbers(),
          history(),
          drawSelection(),
          highlightActiveLine(),
          keymap.of([...defaultKeymap, ...historyKeymap]),
          yaml(),
          syntaxHighlighting(highlight),
          theme,
          readOnlyCompartment.current.of(EditorState.readOnly.of(latest.current.readOnly)),
          EditorView.contentAttributes.of({
            "aria-label": label,
            "aria-multiline": "true",
            spellcheck: "false",
          }),
          EditorView.updateListener.of((update) => {
            if (update.docChanged) {
              onChangeRef.current(update.state.doc.toString());
            }
          }),
        ],
      }),
    });
    view.current = editor;
    return () => {
      editor.destroy();
      view.current = null;
    };
  }, [native, label]);

  useEffect(() => {
    const editor = view.current;
    if (editor !== null && editor.state.doc.toString() !== value) {
      editor.dispatch({ changes: { from: 0, to: editor.state.doc.length, insert: value } });
    }
  }, [value]);

  useEffect(() => {
    view.current?.dispatch({
      effects: readOnlyCompartment.current.reconfigure(EditorState.readOnly.of(readOnly)),
    });
  }, [readOnly]);

  if (native) {
    return (
      <textarea
        aria-label={label}
        className={cn(FRAME, "resize-none p-3 font-mono text-sm outline-none")}
        spellCheck={false}
        readOnly={readOnly}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        wrap="off"
      />
    );
  }
  return <div ref={host} className={FRAME} />;
}
