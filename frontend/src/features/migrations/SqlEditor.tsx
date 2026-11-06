import { useRef, useState, type KeyboardEvent } from 'react';

export function SqlEditor({ value, original, onChange, disabled }: { value: string; original: string; onChange: (value: string) => void; disabled: boolean }) {
  const area = useRef<HTMLTextAreaElement>(null);
  const gutter = useRef<HTMLDivElement>(null);
  const escaped = useRef(false);
  const [caret, setCaret] = useState(0);
  const lines = value.split('\n').length;
  const before = value.slice(0, caret);
  const update = () => {
    if (!area.current) return;
    setCaret(area.current.selectionStart);
    if (gutter.current) gutter.current.scrollTop = area.current.scrollTop;
  };
  const keydown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === 'Escape') { escaped.current = true; return; }
    if (event.key === 'Tab' && !escaped.current && !event.ctrlKey && !event.metaKey && !event.altKey && !event.shiftKey) {
      event.preventDefault();
      const element = event.currentTarget;
      // execCommand mempertahankan undo bawaan textarea.
      if (!document.execCommand || !document.execCommand('insertText', false, '    ')) {
        element.setRangeText('    ', element.selectionStart, element.selectionEnd, 'end');
        onChange(element.value);
      }
      update();
    }
    if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 's') {
      event.preventDefault(); event.currentTarget.form?.requestSubmit();
    }
    if (event.key !== 'Tab') escaped.current = false;
  };
  const modifier = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform) ? '⌘' : 'Ctrl';
  return <>
    <div className={`editor${value !== original ? ' is-dirty' : ''}`}>
      <div className="editor-bar" aria-hidden="true"><i /><i /><i /><b>query.sql</b><span className="dirty">Unsaved</span></div>
      <div ref={gutter} className="editor-gutter" aria-hidden="true">{Array.from({ length: lines }, (_, index) => `${index + 1}\n`).join('')}</div>
      <textarea ref={area} id="sql" name="sql" spellCheck={false} required value={value} disabled={disabled}
        onChange={(event) => { onChange(event.target.value); update(); }} onKeyDown={keydown} onKeyUp={update} onClick={update} onSelect={update} onScroll={update} />
    </div>
    <div className="editor-status" aria-hidden="true">
      <span>{`Ln ${before.split('\n').length}, Col ${before.length - before.lastIndexOf('\n')} · ${lines} line${lines === 1 ? '' : 's'} · ${value.length} chars`}</span>
      <span><kbd>{modifier}</kbd>{' '}<kbd>S</kbd>{' save · '}<kbd>Esc</kbd>{' '}<kbd>Tab</kbd>{' leave editor'}</span>
    </div>
  </>;
}
