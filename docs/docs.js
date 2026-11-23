// Shared behaviour for the vBilling docs pages (every page except index.html).
// It derives the breadcrumbs, the "On this page" column and the previous/next
// links from the sidebar and the headings already in the page, so a page only
// needs its sidebar entry and its content.
(function () {
  var content = document.querySelector('.content');
  var sidebar = document.getElementById('sidebar');
  if (!content || !sidebar) return;

  var HOME_ICON = '<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M10 19v-5h4v5c0 .55.45 1 1 1h3c.55 0 1-.45 1-1v-7h1.7c.46 0 .68-.57.33-.87L12.67 3.6c-.38-.34-.96-.34-1.34 0l-8.36 7.53c-.34.3-.13.87.33.87H5v7c0 .55.45 1 1 1h3c.55 0 1-.45 1-1z"/></svg>';

  function el(tag, attrs, html) {
    var node = document.createElement(tag);
    for (var key in attrs || {}) node.setAttribute(key, attrs[key]);
    if (html != null) node.innerHTML = html;
    return node;
  }

  // ── Mobile sidebar ──
  var toggle = document.getElementById('mobileSidebarToggle');
  if (toggle) {
    toggle.addEventListener('click', function () { sidebar.classList.toggle('open'); });
    document.addEventListener('click', function (e) {
      if (!sidebar.contains(e.target) && !toggle.contains(e.target)) sidebar.classList.remove('open');
    });
  }

  // ── Breadcrumbs: home, sidebar group, this page ──
  var active = sidebar.querySelector('.sidebar-section a.active');
  var h1 = content.querySelector('h1');
  if (active && h1) {
    var label = active.closest('.sidebar-section').querySelector('.sidebar-label');
    var crumbs = el('ul', { 'class': 'breadcrumbs', 'aria-label': 'Breadcrumbs' });
    crumbs.appendChild(el('li', null, '<a href="index.html" aria-label="vBilling home">' + HOME_ICON + '</a>'));
    if (label) crumbs.appendChild(el('li', null, '<span>' + label.textContent + '</span>'));
    crumbs.appendChild(el('li', null, '<span>' + active.textContent + '</span>'));
    content.insertBefore(crumbs, content.firstChild);
  }

  // ── Previous / next, in sidebar order ──
  if (active) {
    var pages = Array.prototype.filter.call(sidebar.querySelectorAll('.sidebar-section a'), function (a) {
      return a.target !== '_blank';
    });
    var at = pages.indexOf(active);
    var prev = pages[at - 1], next = pages[at + 1];
    if (prev || next) {
      var nav = el('nav', { 'class': 'pagination-nav', 'aria-label': 'Docs pages' });
      if (prev) nav.appendChild(el('a', { href: prev.getAttribute('href'), 'class': 'pagination-prev' }, '<small>Previous</small>« ' + prev.textContent));
      if (next) nav.appendChild(el('a', { href: next.getAttribute('href'), 'class': 'pagination-next' }, '<small>Next</small>' + next.textContent + ' »'));
      content.appendChild(nav);
    }
  }

  // ── Tables scroll sideways on narrow screens instead of widening the page ──
  Array.prototype.forEach.call(content.querySelectorAll('table'), function (table) {
    if (table.closest('.config-table-wrap, .hmw-table-wrap, .table-scroll')) return;
    var wrap = el('div', { 'class': 'table-scroll' });
    table.parentNode.insertBefore(wrap, table);
    wrap.appendChild(table);
  });

  // ── Copy buttons on code blocks ──
  if (navigator.clipboard) {
    Array.prototype.forEach.call(content.querySelectorAll('.code-block'), function (block) {
      var code = block.querySelector('code');
      if (!code || block.querySelector('.code-copy')) return;
      var button = el('button', { type: 'button', 'class': 'code-copy', 'aria-label': 'Copy code' }, 'Copy');
      button.addEventListener('click', function () {
        navigator.clipboard.writeText(code.textContent).then(function () {
          button.textContent = 'Copied';
          setTimeout(function () { button.textContent = 'Copy'; }, 1500);
        });
      });
      block.appendChild(button);
    });
  }

  // ── On this page: every h2, and the h3s that sit directly in the page ──
  function headingText(heading) {
    var copy = heading.cloneNode(true);
    Array.prototype.forEach.call(copy.querySelectorAll('.step-marker'), function (n) { n.remove(); });
    return copy.textContent.trim();
  }
  function slug(text) {
    return text.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '') || 'section';
  }

  var headings = Array.prototype.filter.call(content.querySelectorAll('h2, h3'), function (h) {
    return h.parentNode === content;
  });
  if (headings.length < 2) return;

  var toc = el('aside', { 'class': 'toc', 'aria-label': 'On this page' });
  var list = el('ul');
  var sublist = null;
  var links = [];
  headings.forEach(function (heading) {
    var text = headingText(heading);
    if (!heading.id) {
      var id = slug(text), n = 2;
      while (document.getElementById(id)) id = slug(text) + '-' + n++;
      heading.id = id;
    }
    var item = el('li');
    var link = el('a', { href: '#' + heading.id });
    link.textContent = text;
    item.appendChild(link);
    links.push(link);
    if (heading.tagName === 'H2') {
      list.appendChild(item);
      sublist = null;
    } else {
      if (!sublist) {
        sublist = el('ul');
        (list.lastElementChild || list).appendChild(sublist);
      }
      sublist.appendChild(item);
    }
  });
  toc.appendChild(list);
  content.parentNode.insertBefore(toc, content.nextSibling);

  // Highlight the section being read.
  var ticking = false;
  function spy() {
    ticking = false;
    var current = 0;
    for (var i = 0; i < headings.length; i++) {
      if (headings[i].getBoundingClientRect().top <= 96) current = i;
    }
    links.forEach(function (link, i) { link.classList.toggle('active', i === current); });
  }
  window.addEventListener('scroll', function () {
    if (!ticking) { ticking = true; requestAnimationFrame(spy); }
  }, { passive: true });
  spy();
})();
