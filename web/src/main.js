// Vite entry. The vendor libs (marked, DOMPurify, hljs) stay classic scripts
// in index.html so their globals exist before app.js runs.
import './style.css';
import './globals.js';
import './auth.js';
import './app.js';
