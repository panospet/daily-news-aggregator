/* Newsroom interactions. The server remains the source of truth; HTMX handles navigation
   and this file adds read state, drawers, themes, and streaming summaries. */
(function () {
  'use strict';

  function storageGet(key, fallback) {
    try {
      var value = localStorage.getItem(key);
      return value === null ? fallback : JSON.parse(value);
    } catch (_) {
      return fallback;
    }
  }

  function storageSet(key, value) {
    try { localStorage.setItem(key, value); } catch (_) {}
  }

  function readMap() {
    var groups = storageGet('flow-read', {});
    return groups && typeof groups === 'object' ? groups : {};
  }

  function readKey(card) {
    return card.dataset.groupId + ':' + card.dataset.groupCount;
  }

  function markAsRead(card) {
    if (!card || !card.dataset.groupId) return;
    var groups = readMap();
    groups[readKey(card)] = { t: Date.now() };
    storageSet('flow-read', JSON.stringify(groups));
    card.classList.add('card--read');
    var button = card.querySelector('.btn-unread');
    if (button) button.style.display = 'inline-flex';
  }

  function markUnread(card) {
    if (!card || !card.dataset.groupId) return;
    var groups = readMap();
    delete groups[readKey(card)];
    storageSet('flow-read', JSON.stringify(groups));
    card.classList.remove('card--read');
    var button = card.querySelector('.btn-unread');
    if (button) button.style.display = 'none';
  }

  function restoreReadState(root) {
    var groups = readMap();
    (root || document).querySelectorAll('.card[data-group-id]').forEach(function (card) {
      var isRead = Boolean(card.dataset.groupId && groups[readKey(card)]);
      card.classList.toggle('card--read', isRead);
      var button = card.querySelector('.btn-unread');
      if (button) button.style.display = isRead ? 'inline-flex' : 'none';
    });
  }

  function setTheme(mode) {
    if (mode !== 'dark' && mode !== 'light') return;
    document.documentElement.dataset.theme = mode;
    storageSet('flow-theme', mode);
    var button = document.querySelector('[data-theme-toggle]');
    if (button) button.setAttribute('aria-label', 'Switch to ' + (mode === 'dark' ? 'light' : 'dark') + ' theme');
    var themeColor = document.querySelector('meta[name="theme-color"]');
    if (themeColor) themeColor.content = mode === 'dark' ? '#191a16' : '#f4f0e7';
  }

  function toggleMenu(force) {
    var panel = document.getElementById('panel');
    var backdrop = document.getElementById('backdrop');
    var button = document.querySelector('.menu-btn');
    if (!panel) return;
    var open = typeof force === 'boolean' ? force : !panel.classList.contains('panel--open');
    panel.classList.toggle('panel--open', open);
    if (backdrop) backdrop.classList.toggle('backdrop--open', open);
    if (button) button.setAttribute('aria-expanded', String(open));
    document.body.classList.toggle('nav-open', open);
    if (open) {
      var close = panel.querySelector('.panel__close');
      if (close) close.focus();
    }
  }

  function closeMenu(restoreFocus) {
    var wasOpen = document.getElementById('panel')?.classList.contains('panel--open');
    toggleMenu(false);
    if (wasOpen && restoreFocus) document.querySelector('.menu-btn')?.focus();
  }

  function showToast(message, type) {
    var container = document.getElementById('toast-container');
    if (!container) return;
    var toast = document.createElement('div');
    toast.className = 'toast toast--' + (type || 'info');
    toast.textContent = message;
    container.appendChild(toast);
    requestAnimationFrame(function () { toast.classList.add('toast--show'); });
    setTimeout(function () {
      toast.classList.remove('toast--show');
      setTimeout(function () { toast.remove(); }, 250);
    }, 4000);
  }

  function openCard(card) {
    if (!card) return;
    card.classList.add('card--open');
    var toggle = card.querySelector('[data-card-toggle]');
    if (toggle) toggle.setAttribute('aria-expanded', 'true');
    var body = card.querySelector('.card__body');
    if (body && !body.dataset.loaded) {
      body.classList.add('card__body--loading');
      window.htmx.ajax('GET', '/card-body?gid=' + encodeURIComponent(card.dataset.groupId) + '&cat=' + encodeURIComponent(card.dataset.cat || ''), {
        target: body,
        swap: 'innerHTML'
      }).then(function () {
        body.dataset.loaded = '1';
        body.classList.remove('card__body--loading');
      }).catch(function () {
        body.classList.remove('card__body--loading');
      });
    }
  }

  function closeCard(card) {
    if (!card) return;
    card.classList.remove('card--open');
    var toggle = card.querySelector('[data-card-toggle]');
    if (toggle) toggle.setAttribute('aria-expanded', 'false');
    // A summarized topic is read only when its reader is finished with it.
    if (card.dataset.summarized === 'true') markAsRead(card);
  }

  function toggleCard(card) {
    if (!card) return;
    if (card.classList.contains('card--open')) closeCard(card);
    else openCard(card);
  }

  function toggleCategory(header) {
    var section = header.closest('.cat');
    if (!section) return;
    var closed = section.classList.toggle('cat--closed');
    header.setAttribute('aria-expanded', String(!closed));
  }

  function syncNavigation() {
    var current = new URL(window.location.href);
    document.querySelectorAll('[data-nav]').forEach(function (link) {
      var target = new URL(link.href, window.location.origin);
      var active = false;
      if (link.dataset.nav === 'home') active = current.pathname === '/';
      else if (link.dataset.nav === 'category') {
        active = current.pathname === '/filter' && current.searchParams.get('cat') === target.searchParams.get('cat');
      } else {
        active = current.pathname === '/search' && current.searchParams.get('q') === target.searchParams.get('q');
      }
      link.classList.toggle('nav__item--active', active);
      if (active) link.setAttribute('aria-current', 'page');
      else link.removeAttribute('aria-current');
    });

    var search = document.getElementById('story-search');
    if (search && document.activeElement !== search) {
      search.value = current.pathname === '/search' ? (current.searchParams.get('q') || '') : '';
    }
  }

  document.addEventListener('click', function (event) {
    var target;
    if ((target = event.target.closest('[data-theme-toggle]'))) {
      setTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark');
      return;
    }
    if ((target = event.target.closest('.btn-unread'))) {
      event.preventDefault();
      event.stopPropagation();
      markUnread(target.closest('.card'));
      return;
    }
    if ((target = event.target.closest('.btn-pos-feedback'))) {
      var positiveCard = target.closest('.card');
      if (positiveCard) positiveCard.querySelectorAll('.fb').forEach(function (button) { button.style.display = 'none'; });
      return;
    }
    if ((target = event.target.closest('.fb'))) {
      var negativeCard = target.closest('.card');
      var positive = negativeCard && negativeCard.querySelector('.btn-pos-feedback');
      if (positive) positive.style.display = 'none';
      return;
    }
    if (event.target.closest('.menu-btn')) { toggleMenu(); return; }
    if (event.target.closest('.panel__close')) { closeMenu(true); return; }
    if (event.target.closest('#backdrop')) { closeMenu(true); return; }
    if (event.target.closest('.nav__item')) { closeMenu(false); }
    if ((target = event.target.closest('.cat__head'))) { toggleCategory(target); return; }
    if ((target = event.target.closest('[data-card-toggle]'))) { toggleCard(target.closest('.card')); }
  });

  document.addEventListener('keydown', function (event) {
    if (event.key === '/' && !['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement.tagName)) {
      event.preventDefault();
      document.getElementById('story-search')?.focus();
    }
    if (event.key === 'Escape') {
      document.querySelectorAll('.card--open').forEach(closeCard);
      closeMenu(true);
    }
    if ((event.key === 'Enter' || event.key === ' ') && event.target.classList.contains('cat__head')) {
      event.preventDefault();
      toggleCategory(event.target);
    }
  });

  document.addEventListener('htmx:afterSwap', function (event) {
    restoreReadState(event.detail.target || document);
    if (event.detail.target && event.detail.target.id === 'stream') {
      setTimeout(syncNavigation, 0);
    }
  });

  document.addEventListener('htmx:load', function (event) {
    var element = event.detail.elt;
    restoreReadState(element);
    if (element?.classList?.contains('card') && element.classList.contains('card--open') && element.dataset.summarized === 'true') {
      setTimeout(function () { element.scrollIntoView({ behavior: 'smooth', block: 'nearest' }); }, 100);
    }
  });

  document.addEventListener('htmx:poppedState', syncNavigation);
  window.addEventListener('popstate', syncNavigation);
  document.addEventListener('htmx:responseError', function () { showToast('Something went wrong. Please try again.', 'error'); });
  document.addEventListener('htmx:sendError', function () { showToast('Network error. Please check your connection.', 'error'); });
  document.addEventListener('htmx:timeout', function () { showToast('Request timed out. Please try again.', 'error'); });

  // Turn final SSE frames back into the exact verified card returned by the Go handler.
  function parseSSEFrame(raw) {
    var eventName = 'message';
    var data = '';
    raw.split('\n').forEach(function (line) {
      if (line.indexOf('event:') === 0) eventName = line.slice(6).trim();
      else if (line.indexOf('data:') === 0) data += line.slice(5).trim();
    });
    return { event: eventName, data: data };
  }

  function buildStreamNote() {
    var note = document.createElement('em');
    note.className = 'summary__note';
    note.setAttribute('aria-hidden', 'true');
    for (var i = 0; i < 3; i++) {
      var dot = document.createElement('span');
      dot.textContent = '.';
      note.appendChild(dot);
    }
    return note;
  }

  function buildStreamProgress() {
    var progress = document.createElement('div');
    progress.className = 'summary__progress';
    var status = document.createElement('span');
    status.className = 'summary__status';
    status.textContent = 'Preparing your summary…';
    progress.appendChild(status);
    progress.appendChild(buildStreamNote());
    return progress;
  }

  function setStreamStatus(box, message) {
    var status = box && box.querySelector('.summary__status');
    if (status) status.textContent = message;
  }

  function ensureStreamBox(card) {
    var box = card.querySelector('.summary--stream');
    if (box) return box;
    var existing = card.querySelector('.summary');
    if (!existing) return null;
    existing.textContent = '';
    existing.classList.remove('summary--unavailable');
    existing.classList.add('summary--stream');
    existing.setAttribute('role', 'status');
    existing.setAttribute('aria-live', 'polite');
    existing.appendChild(buildStreamProgress());
    var text = document.createElement('span');
    text.className = 'summary__text';
    existing.appendChild(text);
    return existing;
  }

  async function streamSummary(button) {
    var card = button.closest('.card');
    if (!card) return;
    var category = button.dataset.category;
    var groupId = button.dataset.groupId;
    var box = ensureStreamBox(card);
    var textElement = box ? box.querySelector('.summary__text') : null;
    var buttonLabel = button.querySelector('.btn-sum__text');
    var originalLabel = buttonLabel ? buttonLabel.textContent : '';
    var text = '';

    function finish(html) {
      var template = document.createElement('template');
      template.innerHTML = html.trim();
      var node = template.content.firstElementChild;
      if (!node) return false;
      card.replaceWith(node);
      if (window.htmx) window.htmx.process(node);
      restoreReadState(node);
      return true;
    }

    button.disabled = true;
    button.classList.add('htmx-request');
    button.setAttribute('aria-busy', 'true');
    if (buttonLabel) buttonLabel.textContent = 'Summarizing…';
    if (box) {
      box.hidden = false;
      box.classList.remove('summary--failed');
      box.setAttribute('aria-busy', 'true');
      setStreamStatus(box, 'Preparing your summary…');
    }
    if (textElement) textElement.textContent = '';

    try {
      var response = await fetch('/summarize/stream', {
        method: 'POST',
        credentials: 'same-origin',
        headers: {
          'Accept': 'text/event-stream',
          'Cache-Control': 'no-cache',
          'Content-Type': 'application/x-www-form-urlencoded'
        },
        body: new URLSearchParams({ category: category, group_id: groupId })
      });
      if (!response.ok || !response.body) throw new Error('HTTP ' + response.status);

      var reader = response.body.getReader();
      var decoder = new TextDecoder();
      var buffer = '';
      var html = null;
      var failure = null;

      function processFrame(raw) {
        if (!raw.trim()) return;
        var frame = parseSSEFrame(raw);
        var payload = frame.data ? JSON.parse(frame.data) : {};
        if (frame.event === 'status') {
          setStreamStatus(box, payload.message || 'Preparing your summary…');
        } else if (frame.event === 'delta') {
          text += payload.delta || '';
          if (text && textElement) {
            textElement.textContent = text;
            setStreamStatus(box, 'Live draft · sources checked on completion');
          }
        } else if (frame.event === 'reset') {
          text = '';
          if (textElement) textElement.textContent = '';
          setStreamStatus(box, 'Reviewing and refining the draft…');
        } else if (frame.event === 'done') {
          html = payload.html || null;
          failure = payload.error || null;
        }
      }

      function drainFrames(flushRemainder) {
        var separator;
        while ((separator = buffer.match(/\r?\n\r?\n/))) {
          processFrame(buffer.slice(0, separator.index));
          buffer = buffer.slice(separator.index + separator[0].length);
        }
        if (flushRemainder && buffer.trim()) {
          processFrame(buffer);
          buffer = '';
        }
      }

      for (;;) {
        var step = await reader.read();
        if (step.done) break;
        buffer += decoder.decode(step.value, { stream: true });
        drainFrames(false);
      }
      buffer += decoder.decode();
      drainFrames(true);
      if (html && finish(html)) return;
      throw new Error(failure || 'No summary returned');
    } catch (error) {
      // The original endpoint remains a compatible fallback (and handles native Gemini).
      button.disabled = false;
      button.classList.remove('htmx-request');
      button.removeAttribute('aria-busy');
      if (buttonLabel) buttonLabel.textContent = originalLabel;
      if (box) {
        box.hidden = true;
        box.removeAttribute('aria-busy');
      }
      if (window.htmx) {
        window.htmx.ajax('POST', '/summarize', {
          values: { category: category, group_id: groupId },
          target: card,
          swap: 'outerHTML'
        });
      } else if (box) {
        box.hidden = false;
        box.classList.add('summary--failed');
        setStreamStatus(box, 'Summary unavailable');
        if (textElement) textElement.textContent = 'Please try again.';
      }
    }
  }

  document.addEventListener('click', function (event) {
    var button = event.target.closest('[data-stream-summarize]');
    if (!button) return;
    event.preventDefault();
    event.stopPropagation();
    if (!button.disabled) streamSummary(button);
  });

  document.addEventListener('DOMContentLoaded', function () {
    setTheme(document.documentElement.dataset.theme || 'light');
    restoreReadState();
    syncNavigation();
    setInterval(function () {
      if (!document.hidden && !document.querySelector('.card--open') && window.htmx) {
        window.htmx.ajax('GET', '/', { target: '#stream', swap: 'outerHTML' });
      }
    }, 600000);
  });
})();
