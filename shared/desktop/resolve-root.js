// @flow
import path from 'path'
import electron from 'electron'

// Gives a path to the desktop folder in dev/packaged builds. Used to load up runtime assets.

const app = electron.app || electron.remote.app
const root = !__DEV__
  ? path.join(app.getAppPath(), './desktop')
  : path.join(__dirname)

function fix (str) {
  return encodeURI(str && str.replace(new RegExp('\\' + path.sep, 'g'), '/'))
}

export const resolveRoot = (...to: any) => path.resolve(root, ...to)
export const resolveRootAsURL = (...to: any) => `file://${fix(resolveRoot(resolveRoot(...to)))}`
export const resolveImage = (...to: any) => path.resolve(root, '..', 'images', ...to)
export const resolveImageAsURL = (...to: any) => `file://${fix(resolveImage(...to))}`

export default resolveRoot
