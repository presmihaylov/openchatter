/* Participant avatar image lifecycle, shared by room and account views. */

const loadedSources = new Set();

export const avatarImage = (cls, name, source) => {
  const img = document.createElement('img');
  img.className = cls + ' avatar-img avatar-loading';
  img.alt = '';
  img.dataset.avatarState = 'loading';
  img.setAttribute('aria-busy', 'true');

  let settled = false;
  const loaded = () => {
    if (settled) return;
    settled = true;
    if (img.src) loadedSources.add(img.src);
    img.classList.remove('avatar-loading', 'avatar-error');
    img.dataset.avatarState = 'loaded';
    img.alt = name || '';
    img.removeAttribute('aria-busy');
    img.removeAttribute('role');
    img.removeAttribute('aria-label');
  };
  const failed = () => {
    if (settled) return;
    settled = true;
    img.onload = null;
    img.onerror = null;
    img.removeAttribute('src');
    img.classList.remove('avatar-loading');
    img.classList.add('avatar-error');
    img.dataset.avatarState = 'error';
    img.removeAttribute('aria-busy');
    img.setAttribute('role', 'img');
    img.setAttribute('aria-label', name ? `${name} avatar unavailable` : 'Avatar unavailable');
  };
  const use = (url) => {
    if (!url) { failed(); return; }
    if (loadedSources.has(url)) {
      img.classList.remove('avatar-loading');
      img.dataset.avatarState = 'loaded';
      img.alt = name || '';
      img.removeAttribute('aria-busy');
    }
    img.onload = loaded;
    img.onerror = failed;
    img.src = url;
    // A decoded browser/object-URL cache hit is complete synchronously. Clear
    // the loading class before the node is appended, so it cannot flash.
    if (img.complete) {
      if (img.naturalWidth) loaded();
      else failed();
    }
  };

  if (typeof source === 'string') use(source);
  else if (source && typeof source.then === 'function') source.then(use, failed);
  else failed();
  return img;
};
