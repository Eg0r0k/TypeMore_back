import { PassportStrategy } from '@nestjs/passport';
import { Strategy, ExtractJwt } from 'passport-jwt';
import { Injectable } from '@nestjs/common';
import { Request } from 'express';

@Injectable()
export class RtStrategy extends PassportStrategy(Strategy, 'jwt') {
  constructor() {
    super({
      jwtFromRequest: ExtractJwt.fromAuthHeaderAsBearerToken(),
      secretOrKey: process.env.JWT_RT_SECRET,
      passReqToCallback: true
    });
  }

  validate(req: Request, payload: any) {
    const authHeader = req.get('auth');
    if (!authHeader) {
      throw new Error('Auth header is missing');
    }

    const refreshToken = authHeader.replace('Bearer', '').trim();
    return {
      ...payload,
      refreshToken
    };
  }
}
