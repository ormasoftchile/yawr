'use strict';

function value(suffix, environment = process.env) {
  return environment[`YAWR_${suffix}`];
}

function pair(suffix, setting) {
  return { [`YAWR_${suffix}`]: setting };
}

module.exports = { value, pair };
