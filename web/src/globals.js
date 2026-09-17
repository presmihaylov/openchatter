// The first module to run: from here on an uncaught throw or a lost rejection,
// even during boot, lands in a toast and not only in the console.
import { installGlobalHandlers } from './notify.js';
installGlobalHandlers();
