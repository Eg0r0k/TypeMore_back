import { PassportStrategy } from '@nestjs/passport';
import { Strategy, ExtractJwt } from 'passport-jwt';
import { Injectable } from '@nestjs/common';
import { Request } from 'express';

@Injectable()
export class RtStrategy extends PassportStrategy(Strategy, 'jwt-refresh') {
  constructor() {
    super({
      jwtFromRequest: ExtractJwt.fromAuthHeaderAsBearerToken(),
      secretOrKey: process.env.JWT_RT_SECRET,
      passReqToCallback: true
    });
  }

  validate(req: Request, payload: any) {
    const authHeader = req.headers.authorization; // Извлекаем заголовок Authorization
    if (!authHeader) {
      throw new Error('Auth header is missing');
    }

    const [bearer, refreshToken] = authHeader.split(' '); // Разделяем строку заголовка на префикс и токен
    if (bearer !== 'Bearer' || !refreshToken) {
      // Проверяем, что префикс - "Bearer" и токен не пустой
      throw new Error('Invalid auth header');
    }

    return {
      ...payload,
      refreshToken
    };
  }
}
