/* Tiptap WYSIWYG composer. The wire format stays markdown: the editor
   serializes to markdown on send and parses markdown on restore, so agents
   and the feed renderer see exactly what the old textarea produced. */
import { Editor, Extension, InputRule } from '@tiptap/core';
import { searchEmoji, rememberEmoji } from './emoji.js';
import { ICON } from './icons.js';
import StarterKit from '@tiptap/starter-kit';
import HardBreak from '@tiptap/extension-hard-break';
import { Markdown } from '@tiptap/markdown';
import { Placeholder } from '@tiptap/extensions';
import { Plugin } from 'prosemirror-state';
import { Decoration, DecorationSet } from 'prosemirror-view';

const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

// the old textarea sent a soft newline as plain "\n" (feed renders with
// breaks:true); the stock serializer would emit "  \n" and change the wire
const WireBreak = HardBreak.extend({ renderMarkdown: () => '\n' });

const URL_RE = /^https?:\/\/\S+$/;

const escRe = (s) => String(s).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const BROADCASTS = ['channel', 'everyone', 'here'];

// Live @mention chips while you type. Deliberately mirrors renderMarkdown in
// app.js: same known-name list, same longest-first ordering so "@John Smith" is
// not eaten by a "@John" match, same amber-for-you / blue-for-others split. If
// the two ever disagree, the composer is lying about what will be sent.
const MentionHighlight = (getMentionOptions, getMeName, getChannelOptions) => Extension.create({
  name: 'mentionHighlight',
  addProseMirrorPlugins() {
    const decorate = (doc) => {
      const names = (getMentionOptions ? getMentionOptions() : []).map((o) => o.name).concat(BROADCASTS);
      const chans = (getChannelOptions ? getChannelOptions() : []).map((o) => o.name);
      if (!names.length && !chans.length) return DecorationSet.empty;
      const re = new RegExp('@(' + names.slice().sort((a, b) => b.length - a.length).map(escRe).join('|')
        + ')(?![\\w-])', 'g');
      // #channel chips: only channels you are in, same rule as the feed renderer
      const chRe = chans.length
        ? new RegExp('(^|[\\s(\\[])#(' + chans.map(escRe).join('|') + ')(?![\\w-])', 'g') : null;
      const meTargets = new Set(BROADCASTS.concat(getMeName ? [getMeName()] : []));
      const decos = [];
      doc.descendants((node, pos) => {
        if (!node.isText) return;
        let m;
        re.lastIndex = 0;
        while ((m = re.exec(node.text)) !== null) {
          decos.push(Decoration.inline(pos + m.index, pos + m.index + m[0].length,
            { class: 'mention' + (meTargets.has(m[1]) ? ' mention-me' : '') }));
        }
        if (!chRe) return;
        chRe.lastIndex = 0;
        while ((m = chRe.exec(node.text)) !== null) {
          const start = pos + m.index + m[1].length;
          decos.push(Decoration.inline(start, start + 1 + m[2].length, { class: 'chanlink' }));
        }
      });
      return DecorationSet.create(doc, decos);
    };
    return [new Plugin({ props: { decorations: (state) => decorate(state.doc) } })];
  },
});

// createComposer replaces one textarea. The contenteditable element gets the
// old textarea's id plus a `.value` markdown shim, so existing e2e checks and
// callers keep working against the same surface.
export const createComposer = ({ mount, id, placeholder, onSubmit, onChange, getMentionOptions, getMeName, getChannelOptions, slashCommands, browseChannels, onImageFile }) => {
  let editor = null;

  const getMarkdown = () => editor.getMarkdown();
  const setMarkdown = (md) => editor.commands.setContent(md || '', { contentType: 'markdown' });
  // slash parsing wants the literal characters typed, never escaped markdown
  const getPlain = () => editor.state.doc.textBetween(0, editor.state.doc.content.size, '\n', '\n');
  const setPlain = (text) => {
    editor.commands.setContent({
      type: 'doc',
      content: [{ type: 'paragraph', content: text ? [{ type: 'text', text }] : [] }],
    });
    editor.commands.focus('end');
  };

  // ---------- popups: @-mention and /command autocomplete ----------

  const mkBox = (cls) => {
    const box = document.createElement('div');
    box.className = cls + ' hidden';
    mount.parentElement.appendChild(box);
    return box;
  };
  const mentionBox = mkBox('mention-ac');
  const chanBox = mkBox('mention-ac chan-ac');
  const slashBox = mkBox('mention-ac slash-ac');
  const emojiBox = mkBox('mention-ac emoji-ac');

  const mention = { items: [], sel: 0, from: 0 };
  const chan = { items: [], sel: 0, from: 0 };
  const slash = { items: [], sel: 0, browseCache: null };
  const emoji = { items: [], sel: 0, from: 0 };

  const closeMention = () => { mention.items = []; mentionBox.classList.add('hidden'); };
  const closeChan = () => { chan.items = []; chanBox.classList.add('hidden'); };
  const closeSlash = () => { slash.items = []; slash.browseCache = null; slashBox.classList.add('hidden'); };
  const closeEmoji = () => { emoji.items = []; emojiBox.classList.add('hidden'); };
  mention.close = closeMention; chan.close = closeChan; slash.close = closeSlash; emoji.close = closeEmoji;

  const renderList = (box, st, fill, pick) => {
    box.innerHTML = '';
    st.items.forEach((it, i) => {
      const d = document.createElement('div');
      d.className = 'mention-opt' + (i === st.sel ? ' sel' : '');
      fill(d, it);
      // mousedown (not click) so the pick lands before the editor blurs
      d.onmousedown = (ev) => { ev.preventDefault(); pick(it); };
      box.appendChild(d);
    });
    box.classList.toggle('hidden', st.items.length === 0);
  };

  const renderMention = () => renderList(mentionBox, mention, (d, it) => {
    const name = document.createElement('span');
    name.className = 'mention-name';
    // @channel-style rows carry a chrome icon; a member row carries their avatar
    if (it.icon) { name.innerHTML = it.icon; name.appendChild(document.createTextNode(it.name)); }
    if (!it.icon) {
      if (it.avatar) name.appendChild(it.avatar());
      name.appendChild(document.createTextNode(it.name));
    }
    d.appendChild(name);
    // they rank last, so say why instead of letting the sender guess
    if (it.inChannel === false) {
      const hint = document.createElement('span');
      hint.className = 'slash-hint';
      hint.textContent = 'not in channel';
      d.appendChild(hint);
    }
  }, applyMention);
  const renderChan = () => renderList(chanBox, chan, (d, it) => {
    // same monochrome lock as the sidebar, so the picker carries no emoji colour either
    const sigil = '<span class="sigil' + (it.private ? ' sigil-lock' : '') + '" aria-hidden="true">' + (it.private ? ICON.lock : ICON.hash) + '</span>';
    d.innerHTML = sigil + esc(it.name)
      + '<span class="slash-hint">' + esc(it.topic || '') + '</span>';
  }, applyChan);
  const renderEmoji = () => renderList(emojiBox, emoji, (d, it) => {
    d.innerHTML = '<span class="emoji-glyph">' + it.emoji + '</span>:' + esc(it.name) + ':';
  }, applyEmoji);
  const renderSlash = () => renderList(slashBox, slash, (d, it) => {
    d.innerHTML = it.kind === 'cmd'
      ? '/' + esc(it.name) + (it.args ? ' <span class="slash-args">' + esc(it.args) + '</span>' : '') +
        '<span class="slash-hint">' + esc(it.hint) + '</span>'
      : '#' + esc(it.name) + '<span class="slash-hint">' + esc(it.topic || '') + '</span>';
  }, applySlash);

  function applyMention(it) {
    const to = editor.state.selection.from;
    editor.chain().focus()
      .insertContentAt({ from: mention.from, to }, [{ type: 'text', text: '@' + it.name + ' ' }])
      .run();
    closeMention();
  }

  function applyChan(it) {
    const to = editor.state.selection.from;
    editor.chain().focus()
      .insertContentAt({ from: chan.from, to }, [{ type: 'text', text: '#' + it.name + ' ' }])
      .run();
    closeChan();
  }

  // the picker inserts the character itself, so the wire body is plain
  // unicode and agents never have to decode a shortcode
  function applyEmoji(it) {
    const to = editor.state.selection.from;
    editor.chain().focus()
      .insertContentAt({ from: emoji.from, to }, [{ type: 'text', text: it.emoji + ' ' }])
      .run();
    rememberEmoji(it.name);
    closeEmoji();
  }

  function applySlash(it) {
    if (it.kind === 'cmd') {
      setPlain('/' + it.name + (it.args ? ' ' : ''));
      closeSlash();
      // /join immediately re-opens with the channel list
      if (it.name === 'join') updateSlash();
      return;
    }
    setPlain('/join ' + it.name);
    closeSlash();
  }

  // Mirrors Slack: a match on the start of the name beats a match on a later
  // word, somebody who is in this channel beats somebody who is not (they would
  // never see the message), and recency of the conversation breaks the tie.
  const matchScore = (name, typed) => {
    if (!typed) return 1;
    const low = name.toLowerCase();
    if (low.startsWith(typed)) return 2;
    return low.split(/[\s_-]+/).some((w) => w.startsWith(typed)) ? 1 : 0;
  };

  const rankMentions = (opts, typed) => {
    const scored = [];
    opts.forEach((o) => {
      const m = matchScore(o.name, typed);
      if (!m) return;
      let score = m * 1000;
      if (o.inChannel) score += 400;
      if (o.online) score += 60;
      if (o.dormant) score -= 300;   // a real handle nobody is behind any more
      scored.push({ o, score, at: o.talkedAt || '' });
    });
    scored.sort((a, b) => b.score - a.score
      || (a.at < b.at ? 1 : a.at > b.at ? -1 : 0)
      || a.o.name.localeCompare(b.o.name));
    return scored.map((s) => s.o);
  };

  const updateMention = () => {
    const { $from, empty } = editor.state.selection;
    if (!empty || !$from.parent.isTextblock) return closeMention();
    const head = $from.parent.textBetween(0, $from.parentOffset, '\0', '\0');
    const m = head.match(/(^|\s)@([A-Za-z0-9_-]*(?: [A-Za-z0-9_-]*){0,3})$/);
    if (!m) return closeMention();
    mention.from = $from.pos - m[2].length - 1;
    // broadcasts always apply to the channel you are in, so they rank with the
    // members rather than below the whole room
    const opts = getMentionOptions().concat(
      ['channel', 'everyone', 'here'].map((name) => ({ name, icon: ICON.megaphone, inChannel: true })));
    mention.items = rankMentions(opts, m[2].toLowerCase()).slice(0, 8);
    mention.sel = 0;
    renderMention();
  };

  // "#part" completes the channels you are in; a channel you cannot see is
  // never offered, so the popup leaks nothing
  const updateChan = () => {
    if (!getChannelOptions) return;
    const { $from, empty } = editor.state.selection;
    if (!empty || !$from.parent.isTextblock) return closeChan();
    const head = $from.parent.textBetween(0, $from.parentOffset, '\0', '\0');
    const m = head.match(/(^|\s)#([A-Za-z0-9_-]*)$/);
    if (!m) return closeChan();
    chan.from = $from.pos - m[2].length - 1;
    const typed = m[2].toLowerCase();
    chan.items = getChannelOptions()
      .filter((c) => c.name.toLowerCase().includes(typed))
      .sort((a, b) => (b.name.toLowerCase().startsWith(typed) - a.name.toLowerCase().startsWith(typed))
        || a.name.localeCompare(b.name))
      .slice(0, 8);
    chan.sel = 0;
    renderChan();
  };

  // ":wo" opens the picker; the colon must start a word and be followed by at
  // least one character, so "12:45", URLs and a lone ":" never trigger it
  const updateEmoji = () => {
    const { $from, empty } = editor.state.selection;
    if (!empty || !$from.parent.isTextblock) return closeEmoji();
    if ($from.parent.type.name === 'codeBlock') return closeEmoji();
    const head = $from.parent.textBetween(0, $from.parentOffset, '\0', '\0');
    const m = head.match(/(^|\s):([a-z0-9_+-]+)$/i);
    if (!m) return closeEmoji();
    emoji.from = $from.pos - m[2].length - 1;
    emoji.items = searchEmoji(m[2]);
    emoji.sel = 0;
    renderEmoji();
  };

  const updateSlash = () => {
    if (!slashCommands) return;
    const { doc, selection } = editor.state;
    if (!selection.empty) return closeSlash();
    const head = doc.textBetween(0, selection.from, '\n', '\n');
    // stage 2: /join <partial> completes public channel names
    const jm = head.match(/^\/join\s+#?(\S*)$/);
    if (jm) {
      const typed = jm[1].toLowerCase();
      const fill = () => {
        slash.items = (slash.browseCache || []).filter((c) => c.name.toLowerCase().startsWith(typed))
          .map((c) => ({ kind: 'channel', name: c.name, topic: c.topic })).slice(0, 8);
        slash.sel = 0;
        renderSlash();
      };
      if (slash.browseCache) return fill();
      browseChannels().then((chs) => { slash.browseCache = chs; fill(); }).catch(() => {});
      return;
    }
    // stage 1: a lone "/word" at the very start filters command names
    const m = head.match(/^\/([a-z]*)$/i);
    const tail = doc.textBetween(selection.from, doc.content.size, '\n', '\n');
    if (!m || tail.trim() !== '') { closeSlash(); return; }
    const typed = m[1].toLowerCase();
    slash.items = slashCommands.filter((c) => c.name.startsWith(typed)).map((c) => ({ kind: 'cmd', ...c }));
    slash.sel = 0;
    renderSlash();
  };

  const popupKeydown = (box, st, pick, ev) => {
    if (box.classList.contains('hidden') || st.items.length === 0) return false;
    if (ev.key === 'ArrowDown') { st.sel = (st.sel + 1) % st.items.length; return true; }
    if (ev.key === 'ArrowUp') { st.sel = (st.sel + st.items.length - 1) % st.items.length; return true; }
    if (ev.key === 'Enter' || ev.key === 'Tab') { pick(st.items[st.sel]); return true; }
    if (ev.key === 'Escape') { st.close(); return true; }
    return false;
  };

  // ---------- formatting ----------

  const editLink = () => {
    const prev = editor.getAttributes('link').href || '';
    const url = window.prompt('Link URL', prev);
    if (url === null) return true;
    if (!url) { editor.chain().focus().extendMarkRange('link').unsetLink().run(); return true; }
    if (editor.state.selection.empty && !prev) {
      editor.chain().focus()
        .insertContent([{ type: 'text', marks: [{ type: 'link', attrs: { href: url } }], text: url }])
        .run();
      return true;
    }
    editor.chain().focus().extendMarkRange('link').setLink({ href: url }).run();
    return true;
  };

  const ComposerKeys = Extension.create({
    name: 'composerKeys',
    addKeyboardShortcuts() {
      return {
        'Mod-Shift-c': () => this.editor.commands.toggleCodeBlock(),
        'Mod-k': () => editLink(),
        // inside a code block Tab indents (two spaces, the way tablinum does
        // it) instead of moving focus out of the composer
        Tab: () => {
          if (!this.editor.isActive('codeBlock')) return false;
          return this.editor.commands.insertContent('  ');
        },
      };
    },
  });

  // A fence line "```lang" opens a code block on Enter or Shift-Enter, the way
  // tablinum opens one on "```lang ". Without this, Enter sent a message that
  // was just "```" and Shift-Enter left the marker sitting in the paragraph.
  // The marker is swallowed and the language kept. At the start of the
  // paragraph the paragraph itself becomes the block; after a hard break the
  // text before the break stays a paragraph and the block follows it.
  const FENCE_RE = /(^|\n)```([a-z0-9+#-]*)$/i;
  const openFence = (view) => {
    const { $from, empty } = view.state.selection;
    if (!empty || $from.parent.type.name !== 'paragraph') return false;
    if ($from.parentOffset !== $from.parent.content.size) return false;
    const head = $from.parent.textBetween(0, $from.parentOffset, '\n', '\n');
    const m = head.match(FENCE_RE);
    if (!m) return false;
    const language = m[2] || null;
    const markerFrom = $from.pos - (m[0].length - m[1].length);
    if (m[1] === '') {
      editor.chain().deleteRange({ from: markerFrom, to: $from.pos }).setCodeBlock({ language }).run();
      return true;
    }
    editor.chain().deleteRange({ from: markerFrom - 1, to: $from.pos }).splitBlock().setCodeBlock({ language }).run();
    return true;
  };

  // StarterKit's list input rules only fire when the marker starts the whole
  // paragraph, so after Shift-Enter they never fire: the line begins after a
  // hard break. Tiptap renders that break as "\n" in the text it matches, but
  // the break must stay OUT of match[0]: Tiptap re-checks the match against the
  // document with textBetween, where a hard break is the empty string, so a
  // match that includes it never verifies. A lookbehind keeps it out. Then drop
  // the break, split the paragraph there, and wrap the new block in the list.
  const listAfterBreak = (find, toggle) => new InputRule({
    find,
    handler: ({ range, chain }) => {
      chain().deleteRange({ from: range.from - 1, to: range.to }).splitBlock()[toggle]().run();
    },
  });

  const ListAfterBreak = Extension.create({
    name: 'listAfterBreak',
    addInputRules() {
      return [
        listAfterBreak(/(?<=\n)[-+*] $/, 'toggleBulletList'),
        listAfterBreak(/(?<=\n)\d+[.)] $/, 'toggleOrderedList'),
        // the stock "```lang " rule has the same paragraph-start limit
        new InputRule({
          find: /(?<=\n)```([a-z0-9+#-]*) $/i,
          handler: ({ range, chain, match }) => {
            chain().deleteRange({ from: range.from - 1, to: range.to }).splitBlock()
              .setCodeBlock({ language: match[1] || null }).run();
          },
        }),
      ];
    },
  });

  const inList = ($from) => {
    for (let d = $from.depth; d > 0; d -= 1) if ($from.node(d).type.name === 'listItem') return true;
    return false;
  };

  editor = new Editor({
    element: mount,
    contentType: 'markdown',
    content: '',
    extensions: [
      // underline has no markdown form; keep the schema serializable
      StarterKit.configure({ hardBreak: false, underline: false, link: { openOnClick: false } }),
      WireBreak,
      // breaks:true matches the feed renderer, so "\n" round-trips as a break
      Markdown.configure({ markedOptions: { breaks: true } }),
      Placeholder.configure({ placeholder }),
      ComposerKeys,
      ListAfterBreak,
      MentionHighlight(getMentionOptions, getMeName, getChannelOptions),
    ],
    editorProps: {
      handleKeyDown: (view, ev) => {
        if (ev.isComposing) return false;
        if (popupKeydown(mentionBox, mention, applyMention, ev)) { renderMention(); return true; }
        if (popupKeydown(chanBox, chan, applyChan, ev)) { renderChan(); return true; }
        if (popupKeydown(slashBox, slash, applySlash, ev)) { renderSlash(); return true; }
        if (popupKeydown(emojiBox, emoji, applyEmoji, ev)) { renderEmoji(); return true; }
        if (ev.key !== 'Enter') return false;
        if (ev.metaKey || ev.ctrlKey) { onSubmit(); return true; }
        if (openFence(view)) return true;
        if (ev.shiftKey) return false; // hard break
        // Enter makes a newline inside a code block; ⌘Enter still sends
        if (view.state.selection.$from.parent.type.name === 'codeBlock') return false;
        // in a list, Enter makes the next item; an empty item lifts out of the
        // list, so a second Enter sends as usual
        if (inList(view.state.selection.$from)) return false;
        onSubmit();
        return true;
      },
      handlePaste: (view, ev) => {
        const img = [...(ev.clipboardData?.items || [])].find((i) => i.type.startsWith('image/'));
        if (img && onImageFile) {
          const file = img.getAsFile();
          if (file) { ev.preventDefault(); onImageFile(file); return true; }
        }
        const text = (ev.clipboardData?.getData('text/plain') || '').trim();
        // paste a URL over a selection -> link on the selected text
        if (URL_RE.test(text) && !view.state.selection.empty) {
          editor.chain().focus().setLink({ href: text }).run();
          return true;
        }
        return false;
      },
    },
    onUpdate: () => { updateMention(); updateChan(); updateSlash(); updateEmoji(); if (onChange) onChange(); },
    onSelectionUpdate: () => { updateMention(); updateChan(); updateSlash(); updateEmoji(); },
    onBlur: () => setTimeout(() => { closeMention(); closeChan(); closeSlash(); closeEmoji(); }, 100),
  });

  // agents read the raw wire: keep typed text verbatim (the stock serializer
  // backslash-escapes `*_[]~` and HTML-encodes <>&, which would mangle plain
  // chat text like snake_case or "a > b"); marks still serialize as markdown
  editor.storage.markdown.manager.encodeTextForMarkdown = (text) => text;

  const dom = editor.view.dom;
  dom.id = id;
  // compat shim: checks and legacy callers read/write markdown via `.value`
  Object.defineProperty(dom, 'value', { get: getMarkdown, set: setMarkdown });

  const api = {
    editor, dom,
    getMarkdown, setMarkdown, getPlain,
    isEmpty: () => editor.isEmpty,
    clear: () => editor.commands.clearContent(true),
    focus: () => editor.commands.focus('end'),
  };
  dom.__composer = api; // e2e hook
  return api;
};
